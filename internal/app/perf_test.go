//go:build perf

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The docs/09 performance budget, enforced as a CI gate (WP-1.5 AC). This
// measures the *server* budget — request in, response out, over a real HTTP
// listener against the fully-wired product handler, so authn, capability
// gating, RLS, and idempotency are all inside the measurement.
//
// The client-side budget (p95 < 30ms from the local replica) is not measurable
// until the sync client exists in Phase 2 (WP-1.5-decisions.md §1).
//
// Deliberately a Go test rather than k6: it needs no new CI tooling and runs
// against the same harness the rest of the suite uses. k6 arrives with the
// load-test job, when there is a deployed target to point it at.
const (
	readBudget  = 100 * time.Millisecond
	writeBudget = 300 * time.Millisecond

	// samples is enough for a stable p95 without making the gate slow. A
	// warmup pass precedes it so first-call costs (connection setup, statement
	// preparation, page cache) don't land in the distribution.
	samples = 200
	warmup  = 20
)

func TestInteractiveReadMeetsP95Budget(t *testing.T) {
	h := perfEnv(t)

	measure := func() time.Duration {
		start := time.Now()
		status, _ := h.do(t, "GET", "/api/v1/contact", "", nil)
		d := time.Since(start)
		if status != http.StatusOK {
			t.Fatalf("read = %d, want 200", status)
		}
		return d
	}

	for range warmup {
		measure()
	}
	durations := make([]time.Duration, 0, samples)
	for range samples {
		durations = append(durations, measure())
	}

	assertBudget(t, "interactive read (list)", durations, readBudget)
}

func TestWriteMeetsP95Budget(t *testing.T) {
	h := perfEnv(t)

	measure := func(i int) time.Duration {
		body := map[string]any{
			"name":  fmt.Sprintf("Perf Contact %d", i),
			"email": fmt.Sprintf("perf%d@example.com", i),
			"kind":  "customer",
		}
		start := time.Now()
		status, raw := h.do(t, "POST", "/api/v1/contact", idgen.New(), body)
		d := time.Since(start)
		if status != http.StatusCreated {
			t.Fatalf("write = %d, want 201; body=%s", status, raw)
		}
		return d
	}

	for i := range warmup {
		measure(i)
	}
	durations := make([]time.Duration, 0, samples)
	for i := range samples {
		durations = append(durations, measure(warmup+i))
	}

	assertBudget(t, "write (command → committed)", durations, writeBudget)
}

// assertBudget reports the distribution and fails when p95 exceeds budget. It
// always logs p50/p95/max so a regression shows how far over it went, and a
// passing run still leaves headroom visible in CI output.
func assertBudget(t *testing.T, label string, durations []time.Duration, budget time.Duration) {
	t.Helper()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p := func(q float64) time.Duration {
		idx := int(q * float64(len(durations)))
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		return durations[idx]
	}
	p50, p95, max := p(0.50), p(0.95), durations[len(durations)-1]
	t.Logf("%s over %d samples: p50=%v p95=%v max=%v (budget p95 < %v)", label, len(durations), p50, p95, max, budget)
	if p95 > budget {
		t.Errorf("%s p95 = %v, over the docs/09 budget of %v", label, p95, budget)
	}
}

// --- harness ---

type perfHarness struct {
	server *httptest.Server
	token  string
	db     *storage.DB
	tenant tenancy.ID
}

func (h *perfHarness) do(t *testing.T, method, path, idem string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// perfEnv boots a cold SQLite server with one tenant, the contacts module
// enabled, a granted principal, and a realistic-but-small list to read.
func perfEnv(t *testing.T) *perfHarness {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tenant := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(ctx, db, tenant, "perf tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	user := perfUser(t, db, tenant)
	reg, err := capability.Load()
	if err != nil {
		t.Fatalf("capability.Load: %v", err)
	}
	adminCtx := authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user})
	if _, err := capability.Enable(adminCtx, db, reg, tenant, "contacts"); err != nil {
		t.Fatalf("enable contacts: %v", err)
	}

	issued, err := identity.IssueSession(ctx, db, tenant, user, "perf-device")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	// The real gateway stack, with the rate limiter raised out of the way: the
	// default budget (100 rps, burst 200) is smaller than warmup+samples+seed,
	// so a throttled 429 would otherwise be scored as a fast response — or, as
	// it did on CI, fail the run outright. What is under measurement here is
	// how long the server takes to answer, not where the throttle sits.
	cfg, err := gatewayConfig(db)
	if err != nil {
		t.Fatalf("gatewayConfig: %v", err)
	}
	cfg.RateLimit = api.RateLimit{RequestsPerSecond: 1e6, Burst: 1e6}
	srv := httptest.NewServer(api.NewGateway(cfg))
	t.Cleanup(srv.Close)

	ph := &perfHarness{server: srv, token: issued.Token, db: db, tenant: tenant}

	// A list route over an empty table measures nothing interesting; seed a
	// page's worth of rows so the read budget covers real serialization.
	for i := range 50 {
		status, raw := ph.do(t, "POST", "/api/v1/contact", idgen.New(), map[string]any{
			"name":  fmt.Sprintf("Seed Contact %d", i),
			"email": fmt.Sprintf("seed%d@example.com", i),
			"kind":  "customer",
		})
		if status != http.StatusCreated {
			t.Fatalf("seed contact = %d; body=%s", status, raw)
		}
	}
	return ph
}

func perfUser(t *testing.T, db *storage.DB, tenant tenancy.ID) identity.UserID {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("perf")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "perf-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range []string{"create", "read", "update", "delete"} {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Contact", action, ""); err != nil {
			t.Fatalf("GrantPermission: %v", err)
		}
	}
	if err := authz.GrantPermission(ctx, db, tenant, role, "capability", "manage", ""); err != nil {
		t.Fatalf("GrantPermission(capability): %v", err)
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return user.ID
}

// A dashboard is an interactive read like any other, and it is the most
// expensive one this build has: it evaluates several metrics twice (current
// period and prior), and the period-scoped ones fold the tenant's event log
// rather than reading the incremental projection (WP-1.8-decisions.md §1).
//
// Measuring it keeps that documented ceiling honest — the day the fold stops
// fitting the budget, this gate says so instead of a user noticing.
func TestDashboardMeetsP95Budget(t *testing.T) {
	h := perfEnvWithBook(t)

	measure := func() time.Duration {
		start := time.Now()
		status, raw := h.do(t, "GET", "/api/v1/dashboards/ceo?currency=EUR", "", nil)
		d := time.Since(start)
		if status != http.StatusOK {
			t.Fatalf("dashboard = %d, want 200; body=%s", status, raw)
		}
		return d
	}

	for range warmup {
		measure()
	}
	durations := make([]time.Duration, 0, samples)
	for range samples {
		durations = append(durations, measure())
	}

	assertBudget(t, "interactive read (dashboard)", durations, readBudget)
}

// perfEnvWithBook is perfEnv plus the demo book, so the dashboard has real
// entries to fold rather than measuring an empty ledger.
func perfEnvWithBook(t *testing.T) *perfHarness {
	t.Helper()
	h := perfEnv(t)

	ctx := context.Background()
	email := idgen.New() + "@example.com"
	hash, err := identity.HashPassword("perf")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, h.db, h.tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, h.db, h.tenant, "perf-book-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for object, actions := range map[string][]string{
		"Account":      {"create", "read"},
		"Contact":      {"create", "read", "update"},
		"Period":       {"create", "read"},
		"Invoice":      {"create", "read", "post"},
		"Receipt":      {"create", "read", "post"},
		"JournalEntry": {"post", "read"},
	} {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, h.db, h.tenant, role, object, action, ""); err != nil {
				t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, h.db, h.tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := SeedDemo(ctx, h.db, DemoInput{
		Tenant: string(h.tenant), Email: email, Currency: "EUR",
		Today: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// Measure as the user who has the ledger grants: the contact-only perf user
	// would be served a dashboard with every tile omitted, which is fast for the
	// wrong reason.
	issued, err := identity.IssueSession(ctx, h.db, h.tenant, user.ID, "perf-book-device")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	h.token = issued.Token
	return h
}

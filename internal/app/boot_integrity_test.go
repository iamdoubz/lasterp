//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/integrity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// This is the WP-1.4b boot-assembly e2e: it drives the full invoice lifecycle
// (draft → post → GL → PDF) over HTTP against a cold-booted server on both
// dialects, exercising authn (bearer session), idempotency keys, capability
// gating, the tax/FX authz seam, and the action surface. It runs under the
// locked-down app-role posture on Postgres (mirroring modules/ledger) so boot
// assembly is proven to compose with DB role separation (INV-F5), not just in a
// superuser sandbox. Invariants touched: INV-T1 (tenant isolation), INV-T2
// (authz on writes), INV-F1 (balanced GL), INV-F2 (posted-doc immutability),
// INV-F5 (post only via pipeline), INV-F6 (gapless number at acceptance).

// --- HTTP client helpers ---

type env struct {
	t      *testing.T
	server *httptest.Server
	db     *storage.DB
	tenant tenancy.ID
	token  string
}

// call issues an HTTP request against the booted server. idem is the
// Idempotency-Key ("" to omit); body is JSON-encoded ("" for none). It returns
// the status, response bytes, and the parsed JSON object (nil for non-JSON).
func (e *env) call(method, path, token, idem string, body any) (int, []byte, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, raw, parsed
}

// post is a convenience for an authenticated write with a fresh idempotency key.
func (e *env) post(path string, body any) (int, []byte, map[string]any) {
	return e.call("POST", path, e.token, idgen.New(), body)
}

func (e *env) get(path string) (int, []byte, map[string]any) {
	return e.call("GET", path, e.token, "", nil)
}

func mustField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok || s == "" {
		t.Fatalf("expected string field %q in %v", key, m)
	}
	return s
}

// --- the tests ---

// TestColdBootMigrates is AC1: a cold Open against a fresh database boots
// migrated and the wired handler serves health + a non-empty OpenAPI document.
func TestColdBootMigrates(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			h, err := Handler(db)
			if err != nil {
				t.Fatalf("Handler: %v", err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
			}

			resp, err = srv.Client().Get(srv.URL + "/api/v1/openapi.json")
			if err != nil {
				t.Fatalf("GET openapi: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var spec map[string]any
			if err := json.Unmarshal(raw, &spec); err != nil {
				t.Fatalf("openapi not JSON: %v", err)
			}
			paths, _ := spec["paths"].(map[string]any)
			// AC3: the action routes are documented.
			for _, p := range []string{"/api/v1/invoices/{id}/post", "/api/v1/periods/{id}/close", "/api/v1/taxrates"} {
				if _, ok := paths[p]; !ok {
					t.Errorf("OpenAPI missing action path %q", p)
				}
			}
		})
	}
}

// TestInvoiceLifecycleOverHTTP is AC2: draft → post → GL → PDF driven entirely
// over HTTP with authn + idempotency keys, on both dialects.
func TestInvoiceLifecycleOverHTTP(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// Chart of accounts (generic Account CRUD).
			arID := e.createAccount("1100", "Accounts Receivable", "asset")
			revID := e.createAccount("4000", "Sales Revenue", "income")
			taxID := e.createAccount("2200", "Tax Payable", "liability")

			// Contact (generic Contact CRUD).
			st, _, contact := e.post("/api/v1/contact", map[string]any{
				"name": "Acme Co", "email": "ap@acme.example", "kind": "customer",
			})
			if st != http.StatusCreated {
				t.Fatalf("create contact status = %d", st)
			}
			contactID := mustField(t, contact, "id")

			// Fiscal period (bespoke action).
			st, _, period := e.post("/api/v1/periods", map[string]any{
				"code": "2026-07", "start_date": "2026-07-01", "end_date": "2026-07-31",
			})
			if st != http.StatusCreated {
				t.Fatalf("create period status = %d", st)
			}
			if mustField(t, period, "status") != ledger.PeriodOpen {
				t.Fatalf("new period not open: %v", period)
			}

			// Tax rate (reference-data admin — authorized write, AC's positive case).
			st, body, _ := e.post("/api/v1/taxrates", map[string]any{
				"jurisdiction": "US-CA", "category": "sales", "rate": "0.10",
				"rounding": "half_even", "as_of": "2026-01-01", "name": "CA sales",
			})
			if st != http.StatusCreated {
				t.Fatalf("create tax rate status = %d: %s", st, body)
			}

			// Draft invoice (bespoke — Invoice is NOT generic CRUD).
			draft := map[string]any{
				"contact_id": contactID, "currency": "USD", "issue_date": "2026-07-15",
				"ar_account": arID, "tax_account": taxID,
				"lines": []map[string]any{{
					"description": "Consulting", "quantity": 1, "unit_price_minor": 10000,
					"revenue_account": revID, "tax_jurisdiction": "US-CA", "tax_category": "sales",
				}},
			}
			st, body, created := e.post("/api/v1/invoices", draft)
			if st != http.StatusCreated {
				t.Fatalf("create draft status = %d: %s", st, body)
			}
			invID := mustField(t, created, "ID")
			if created["Status"] != invoicing.StatusDraft {
				t.Fatalf("draft status = %v, want draft", created["Status"])
			}

			// Post the invoice (draft → post → GL). Fixed idempotency key so we can
			// replay it below.
			postKey := idgen.New()
			st, body, posted := e.call("POST", "/api/v1/invoices/"+invID+"/post", e.token, postKey, map[string]any{"period": "2026-07"})
			if st != http.StatusOK {
				t.Fatalf("post invoice status = %d: %s", st, body)
			}
			if posted["Status"] != invoicing.StatusPosted {
				t.Fatalf("posted status = %v, want posted", posted["Status"])
			}
			glID := mustField(t, posted, "GLEntryID")
			number := mustField(t, posted, "Number")
			if int64(posted["GrossMinor"].(float64)) != 11000 {
				t.Fatalf("gross = %v, want 11000 (10000 net + 10%% tax)", posted["GrossMinor"])
			}

			// GL entry is reachable and balances (INV-F1): the event-sourced read route.
			st, body, entry := e.get("/api/v1/journalentries/" + glID)
			if st != http.StatusOK {
				t.Fatalf("get journal entry status = %d: %s", st, body)
			}
			assertBalanced(t, entry)

			// PDF renders (AC2 final step).
			st, pdf, _ := e.get("/api/v1/invoices/" + invID + "/pdf")
			if st != http.StatusOK {
				t.Fatalf("get pdf status = %d", st)
			}
			if !bytes.HasPrefix(pdf, []byte("%PDF")) {
				t.Fatalf("pdf does not start with %%PDF: %q", pdf[:min(8, len(pdf))])
			}

			// Idempotent replay: same key → identical result, no second effect (INV-E4).
			st, replay, replayed := e.call("POST", "/api/v1/invoices/"+invID+"/post", e.token, postKey, map[string]any{"period": "2026-07"})
			if st != http.StatusOK {
				t.Fatalf("replay post status = %d", st)
			}
			if replayed["Number"] != number || replayed["GLEntryID"] != glID {
				t.Fatalf("replay differs: %s", replay)
			}
		})
	}
}

// TestUnauthorizedTaxRateWriteRejected is AC4: a principal lacking
// TaxRate:manage is refused (403) at the API — the reference-data authz seam
// deferred from WP-1.1/1.3 now enforced (INV-T2).
func TestUnauthorizedTaxRateWriteRejected(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			// A second principal with capability:manage but NOT TaxRate:manage.
			weak := e.issueUser(t, map[string][]string{"capability": {"manage"}})

			st, body, _ := e.call("POST", "/api/v1/taxrates", weak, idgen.New(), map[string]any{
				"jurisdiction": "US-NY", "category": "sales", "rate": "0.08", "as_of": "2026-01-01",
			})
			if st != http.StatusForbidden {
				t.Fatalf("unauthorized tax-rate write status = %d, want 403: %s", st, body)
			}

			// Unauthenticated is refused too (401), never a silent write.
			st, _, _ = e.call("POST", "/api/v1/taxrates", "", idgen.New(), map[string]any{
				"jurisdiction": "US-NY", "category": "sales", "rate": "0.08", "as_of": "2026-01-01",
			})
			if st != http.StatusUnauthorized {
				t.Fatalf("unauthenticated tax-rate write status = %d, want 401", st)
			}
		})
	}
}

// TestInvoiceHasNoGenericCrudRoute proves the decision that closes the INV-F5/F6
// hole: Invoice is not registered as a generic CRUD object, so there is no
// unguarded create/patch that could mint a "posted" invoice bypassing the
// posting pipeline. The bespoke POST exists; a PATCH of arbitrary fields (the
// generic-CRUD verb) does not.
func TestInvoiceHasNoGenericCrudRoute(t *testing.T) {
	e := seed(t, sqliteBootDB(t))
	// The generic collection path /api/v1/invoice (singular, CRUD naming) must
	// not exist — only the bespoke /api/v1/invoices/... routes do.
	st, _, _ := e.call("GET", "/api/v1/invoice", e.token, "", nil)
	if st != http.StatusNotFound {
		t.Fatalf("generic /api/v1/invoice GET status = %d, want 404 (Invoice must not be generic CRUD)", st)
	}
}

// TestCapabilityEndpointsOverHTTP exercises the capability admin surface.
func TestCapabilityEndpointsOverHTTP(t *testing.T) {
	e := seed(t, sqliteBootDB(t))

	st, body, list := e.get("/api/v1/capabilities")
	if st != http.StatusOK {
		t.Fatalf("list capabilities status = %d: %s", st, body)
	}
	if _, ok := list["enabled"]; !ok {
		t.Fatalf("capabilities response missing 'enabled': %s", body)
	}

	// Disable then re-enable a leaf module (crm has no enabled dependents here).
	st, _, _ = e.post("/api/v1/capabilities/crm/enable", nil)
	if st != http.StatusOK {
		t.Fatalf("enable crm status = %d", st)
	}
	st, _, _ = e.post("/api/v1/capabilities/crm/disable", nil)
	if st != http.StatusOK {
		t.Fatalf("disable crm status = %d", st)
	}
}

func assertBalanced(t *testing.T, entry map[string]any) {
	t.Helper()
	lines, ok := entry["Lines"].([]any)
	if !ok || len(lines) == 0 {
		t.Fatalf("entry has no lines: %v", entry)
	}
	var debit, credit int64
	for _, l := range lines {
		lm := l.(map[string]any)
		debit += int64(lm["Debit"].(float64))
		credit += int64(lm["Credit"].(float64))
	}
	if debit != credit {
		t.Fatalf("entry not balanced (INV-F1): Σdebit=%d Σcredit=%d", debit, credit)
	}
	if debit != 11000 {
		t.Fatalf("entry debit total = %d, want 11000", debit)
	}
}

// createAccount creates a chart-of-accounts entry via the generic Account CRUD
// route and returns its id.
func (e *env) createAccount(code, name, accountType string) string {
	e.t.Helper()
	st, body, rec := e.post("/api/v1/account", map[string]any{
		"code": code, "name": name, "type": accountType, "currency": "USD",
	})
	if st != http.StatusCreated {
		e.t.Fatalf("create account %s status = %d: %s", code, st, body)
	}
	return mustField(e.t, rec, "id")
}

// --- seeding + harness ---

// seed provisions a tenant, enables the modules the lifecycle needs, and issues
// a session for a fully-privileged principal. It returns an env bound to a live
// httptest server over the wired product handler.
func seed(t *testing.T, db *storage.DB) *env {
	t.Helper()
	ctx := context.Background()
	tenant := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(ctx, db, tenant, "boot test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	h, err := Handler(db)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	e := &env{t: t, server: srv, db: db, tenant: tenant}

	// Enable every module the lifecycle touches (capability gate, ADR-018).
	reg, err := capability.Load()
	if err != nil {
		t.Fatalf("capability.Load: %v", err)
	}
	adminCtx := e.actorCtx(t, map[string][]string{"capability": {"manage"}})
	for _, module := range []string{"contacts", "ledger", "tax-engine", "invoicing"} {
		if _, err := capability.Enable(adminCtx, db, reg, tenant, module); err != nil {
			t.Fatalf("enable %s: %v", module, err)
		}
	}

	e.token = e.issueUser(t, fullGrants())
	return e
}

func fullGrants() map[string][]string {
	return map[string][]string{
		"Account":      {"create", "read", "update", "delete"},
		"Contact":      {"create", "read", "update", "delete"},
		"Period":       {"create", "read", "update"},
		"Invoice":      {"create", "read", "update", "post"},
		"JournalEntry": {"post", "read", "reverse"},
		"capability":   {"manage"},
		"TaxRate":      {"manage"},
		"FxRate":       {"manage"},
	}
}

// issueUser creates a user with a role carrying grants and returns a bearer
// session token for it.
func (e *env) issueUser(t *testing.T, grants map[string][]string) string {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, e.db, e.tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	grantRole(t, e.db, e.tenant, user.ID, grants)
	issued, err := identity.IssueSession(ctx, e.db, e.tenant, user.ID, "boot-test-device")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	return issued.Token
}

// actorCtx creates a user with grants and returns a context bound with its
// actor (for in-process seeding calls like capability.Enable).
func (e *env) actorCtx(t *testing.T, grants map[string][]string) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, e.db, e.tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	grantRole(t, e.db, e.tenant, user.ID, grants)
	return authz.WithActor(ctx, authz.Actor{TenantID: e.tenant, UserID: user.ID})
}

func grantRole(t *testing.T, db *storage.DB, tenant tenancy.ID, user identity.UserID, grants map[string][]string) {
	t.Helper()
	ctx := context.Background()
	role, err := authz.CreateRole(ctx, db, tenant, "role-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for object, actions := range grants {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, db, tenant, role, object, action, ""); err != nil {
				t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
}

// bootDBs returns a fully cold-booted DB per dialect. SQLite boots via the
// single-role Open path; Postgres boots under the locked-down app-role posture
// (migrate as superuser, register/seed as a NOSUPERUSER NOBYPASSRLS app role
// with the append-only/pipeline grants applied), mirroring modules/ledger, so
// the boot assembly is proven to compose with DB role separation (INV-F5).
func bootDBs(t *testing.T) map[string]*storage.DB {
	t.Helper()
	return map[string]*storage.DB{
		"sqlite":   sqliteBootDB(t),
		"postgres": postgresBootDB(t),
	}
}

func sqliteBootDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "lasterp.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	if err := Setup(ctx, db); err != nil {
		t.Fatalf("setup sqlite: %v", err)
	}
	return db
}

func postgresBootDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_boot"),
		tcpostgres.WithUsername("lasterp"),
		tcpostgres.WithPassword("lasterp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// Migrate as the privileged (owner) role: the SECURITY DEFINER pipeline
	// functions must be owned by a role that keeps INSERT on events.
	superDB, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres (superuser): %v", err)
	}
	defer func() { _ = superDB.Close() }()
	if err := Migrate(ctx, superDB); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}

	const appUser, appPassword = "lasterp_app", "lasterp_app"
	for _, stmt := range []string{
		`CREATE ROLE ` + appUser + ` LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE, CREATE ON SCHEMA public TO ` + appUser,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appUser,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appUser,
	} {
		if _, err := superDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	if err := integrity.EnforceAppendOnlyGrants(ctx, superDB, appUser); err != nil {
		t.Fatalf("EnforceAppendOnlyGrants: %v", err)
	}
	if err := integrity.EnforceLedgerPipelineGrants(ctx, superDB, appUser); err != nil {
		t.Fatalf("EnforceLedgerPipelineGrants: %v", err)
	}

	appDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	appDSN.User = url.UserPassword(appUser, appPassword)
	appDB, err := postgres.Open(appDSN.String())
	if err != nil {
		t.Fatalf("open postgres (app role): %v", err)
	}
	t.Cleanup(func() { _ = appDB.Close() })

	// Register modules + seed under the restricted app role (it owns the obj_*
	// tables it creates).
	if err := Setup(ctx, appDB); err != nil {
		t.Fatalf("setup postgres (app role): %v", err)
	}
	return appDB
}

//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/modules/reporting"
)

// namedRoleUser creates a user whose role has a specific *name*, which is what
// pack suggestion keys off (grantRole's generated names never match a pack).
// It returns the user's email and a session token.
func namedRoleUser(t *testing.T, e *env, role string, grants map[string][]string) (string, string) {
	t.Helper()
	ctx := context.Background()
	email := idgen.New() + "@example.com"
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, e.db, e.tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	roleID, err := authz.CreateRole(ctx, e.db, e.tenant, role, false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for object, actions := range grants {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, e.db, e.tenant, roleID, object, action, ""); err != nil {
				t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, e.db, e.tenant, user.ID, roleID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	issued, err := identity.IssueSession(ctx, e.db, e.tenant, user.ID, "dashboard-test-device")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	return email, issued.Token
}

// WP-1.8 AC, server half: a fresh tenant seeded with the demo book serves a live
// role dashboard whose numbers come from the ledger, with real comparisons.

// seededTenant bootstraps a tenant through the product's own paths and fills it
// with the demo book, exactly as an operator would.
func seededTenant(t *testing.T, db *storage.DB) *env {
	t.Helper()
	e := seed(t, db)

	// The seeder is attributed to a real user holding the grants the writes
	// need — there is no privileged seeding identity (INV-T2/T4).
	email, token := namedRoleUser(t, e, "administrator", fullGrants())
	e.token = token
	if err := SeedDemo(context.Background(), db, DemoInput{
		Tenant: string(e.tenant), Email: email, Currency: "EUR",
		// Anchored so the seeded periods are stable regardless of when the
		// suite runs.
		Today: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	return e
}

func decodeDashboard(t *testing.T, raw []byte) reporting.Rendered {
	t.Helper()
	var out reporting.Rendered
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode dashboard: %v (%s)", err, raw)
	}
	return out
}

// TestRoleDashboardIsLiveOnASeededTenant is the acceptance criterion.
func TestRoleDashboardIsLiveOnASeededTenant(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seededTenant(t, db)

			status, raw, _ := e.get("/api/v1/dashboards/ceo?currency=EUR")
			if status != http.StatusOK {
				t.Fatalf("render ceo dashboard = %d; body=%s", status, raw)
			}
			d := decodeDashboard(t, raw)

			if d.Headline == nil {
				t.Fatal("dashboard has no headline tile")
			}
			if d.Headline.Metric != "revenue" {
				t.Errorf("headline = %q, want revenue", d.Headline.Metric)
			}
			if d.Headline.Value <= 0 {
				t.Errorf("headline value = %d, want the seeded revenue — a live dashboard is not zeroes", d.Headline.Value)
			}
			if d.Period == "" {
				t.Error("dashboard does not say which period it covers")
			}
			if d.Currency != "EUR" {
				t.Errorf("currency = %q, want EUR", d.Currency)
			}

			// docs/21 §3: "a lone 4.2M is impossible by default". The demo book
			// posts into both periods precisely so this is not vacuous.
			if d.Headline.Comparison == nil {
				t.Fatal("headline card has no comparison")
			}
			if d.Headline.Comparison.Value <= 0 {
				t.Errorf("comparison value = %d, want the prior period's revenue", d.Headline.Comparison.Value)
			}
			if d.Headline.Comparison.DeltaMinor == 0 {
				t.Error("comparison delta is zero — the two periods were folded as one")
			}
			if d.Headline.Comparison.Basis != reporting.BasisPriorPeriod {
				t.Errorf("comparison basis = %q", d.Headline.Comparison.Basis)
			}

			if len(d.Cards) == 0 {
				t.Fatal("dashboard rendered no supporting tiles")
			}
			for _, c := range d.Cards {
				if c.Label == "" {
					t.Errorf("card %q has no label", c.Metric)
				}
				if c.Grain == "" {
					t.Errorf("card %q does not declare its grain", c.Metric)
				}
			}
		})
	}
}

// A flow tile must report the period's movement, not the book's whole history —
// the difference between "revenue this month" and "revenue ever" is the entire
// point of the grain.
func TestFlowTileIsPeriodScoped(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	status, raw, _ := e.get("/api/v1/dashboards/ceo?currency=EUR")
	if status != http.StatusOK {
		t.Fatalf("render = %d; body=%s", status, raw)
	}
	current := decodeDashboard(t, raw)

	// The all-time metric route ignores periods, so it must exceed one period's
	// revenue on a book with entries in two.
	status, raw, _ = e.get("/api/v1/metrics/revenue?currency=EUR")
	if status != http.StatusOK {
		t.Fatalf("metric = %d; body=%s", status, raw)
	}
	var allTime reporting.Value
	if err := json.Unmarshal(raw, &allTime); err != nil {
		t.Fatalf("decode metric: %v", err)
	}

	if current.Headline.Value >= allTime.Value {
		t.Errorf("period revenue %d is not less than all-time revenue %d — the tile is not period-scoped",
			current.Headline.Value, allTime.Value)
	}
	if current.Headline.Value+current.Headline.Comparison.Value != allTime.Value {
		t.Errorf("period revenue %d + prior %d != all-time %d — the periods do not partition the book",
			current.Headline.Value, current.Headline.Comparison.Value, allTime.Value)
	}
}

// INV-T1: a viewer who cannot read invoices must not learn AR figures from a
// dashboard. The tile is omitted and named, never rendered as a zero — a zero is
// a number, and an invented one at that.
func TestDashboardOmitsTilesTheViewerMayNotSee(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seededTenant(t, db)

			// A ledger reader with no invoice access at all. Period:read is
			// the dashboard's entry permission — rendering has to resolve which
			// fiscal period it is reporting on (decisions §3).
			restricted := e.issueUser(t, map[string][]string{
				"JournalEntry": {"read"},
				"Account":      {"read"},
				"Period":       {"read"},
			})
			status, raw, _ := e.call("GET", "/api/v1/dashboards/ceo?currency=EUR", restricted, "", nil)
			if status != http.StatusOK {
				t.Fatalf("render for restricted viewer = %d; body=%s", status, raw)
			}
			d := decodeDashboard(t, raw)

			for _, c := range d.Cards {
				if c.Metric == "ar_outstanding" {
					t.Errorf("AR tile rendered for a viewer with no Invoice:read grant (value %d)", c.Value)
				}
			}
			var omitted bool
			for _, name := range d.Omitted {
				if name == "ar_outstanding" {
					omitted = true
				}
			}
			if !omitted {
				t.Errorf("AR tile was neither rendered nor named as omitted: %+v", d.Omitted)
			}

			// The ledger tiles they *can* see still render, so the dashboard
			// degrades rather than disappearing.
			if d.Headline == nil || d.Headline.Value == 0 {
				t.Error("restricted viewer lost the headline they are entitled to")
			}
		})
	}
}

// A viewer who cannot see the headline is not offered the pack at all: a
// dashboard whose main number is missing is not that dashboard.
func TestCatalogMarksWhatTheViewerCanRender(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	status, raw, _ := e.get("/api/v1/dashboards")
	if status != http.StatusOK {
		t.Fatalf("list dashboards = %d; body=%s", status, raw)
	}
	var payload struct {
		Data []reporting.Listing `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode listing: %v", err)
	}

	byName := map[string]reporting.Listing{}
	for _, l := range payload.Data {
		byName[l.Name] = l
	}
	for _, name := range []string{"ceo", "cfo", "ar"} {
		l, ok := byName[name]
		if !ok {
			t.Errorf("pack %q missing from the catalog", name)
			continue
		}
		if !l.Available {
			t.Errorf("pack %q is not available to a full administrator", name)
		}
	}
	// The AP pack ships gated on a capability that does not exist yet: listed
	// (so it is discoverable), never available (so it cannot render empty).
	if ap, ok := byName["ap"]; !ok {
		t.Error("the gated AP pack is absent from the catalog entirely")
	} else if ap.Available {
		t.Error("the AP pack claims to be available before payables ships")
	}

	// An administrator holds the "administrator" role, which every pack lists.
	if !byName["ceo"].Suggested {
		t.Error("no pack was suggested for the administrator role")
	}
}

// Rendering a gated pack fails cleanly rather than producing an empty grid.
func TestGatedDashboardDoesNotRender(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	if status, raw, _ := e.get("/api/v1/dashboards/ap?currency=EUR"); status != http.StatusNotFound {
		t.Errorf("render gated pack = %d, want 404; body=%s", status, raw)
	}
	if status, raw, _ := e.get("/api/v1/dashboards/nope?currency=EUR"); status != http.StatusNotFound {
		t.Errorf("render unknown pack = %d, want 404; body=%s", status, raw)
	}
	if status, raw, _ := e.get("/api/v1/dashboards/ceo"); status != http.StatusBadRequest {
		t.Errorf("render without a currency = %d, want 400; body=%s", status, raw)
	}
}

// A tenant with no fiscal calendar cannot be reported on by period. It answers
// 422 rather than rendering something meaningless or a bare 500.
func TestDashboardOnABookWithNoPeriods(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db) // bootstrapped, but never seeded with a book

	status, raw, _ := e.get("/api/v1/dashboards/ceo?currency=EUR")
	if status != http.StatusUnprocessableEntity {
		t.Errorf("render on an empty book = %d, want 422; body=%s", status, raw)
	}
}

// An explicit period renders that period, so a reader can look at last month.
func TestDashboardHonoursAnExplicitPeriod(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	status, raw, _ := e.get("/api/v1/dashboards/ceo?currency=EUR&period=2026-06")
	if status != http.StatusOK {
		t.Fatalf("render prior period = %d; body=%s", status, raw)
	}
	d := decodeDashboard(t, raw)
	if d.Period != "2026-06" {
		t.Errorf("period = %q, want 2026-06", d.Period)
	}
	// It is the first seeded period, so there is nothing before it to compare
	// against — and the card says so instead of inventing a zero baseline.
	if d.Headline.Comparison != nil {
		t.Errorf("first period rendered a comparison against %+v", d.Headline.Comparison)
	}

	if status, raw, _ := e.get("/api/v1/dashboards/ceo?currency=EUR&period=1999-01"); status != http.StatusUnprocessableEntity {
		t.Errorf("render unknown period = %d, want 422; body=%s", status, raw)
	}
}

// Rendering resolves the tenant's fiscal calendar to decide which period it is
// reporting on, so Period:read is the dashboard's entry permission. A viewer
// without it is refused outright rather than shown a dashboard scoped to a
// period they were not allowed to look up.
func TestDashboardRequiresReadingTheFiscalCalendar(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	noCalendar := e.issueUser(t, map[string][]string{
		"JournalEntry": {"read"},
		"Account":      {"read"},
	})
	status, raw, _ := e.call("GET", "/api/v1/dashboards/ceo?currency=EUR", noCalendar, "", nil)
	if status != http.StatusForbidden {
		t.Errorf("render without Period:read = %d, want 403; body=%s", status, raw)
	}

	// The catalog still lists what they could render with the right grants:
	// knowing a dashboard exists is not knowing what is on it.
	if status, raw, _ := e.call("GET", "/api/v1/dashboards", noCalendar, "", nil); status != http.StatusOK {
		t.Errorf("list dashboards without Period:read = %d, want 200; body=%s", status, raw)
	}
}

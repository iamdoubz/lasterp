//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// WP-1.6 AC: "permission-leak suite green."
//
// docs/21 §1: "The builder cannot leak what the user cannot read — enforced in
// the query engine (docs/19 INV-T1), not the UI." A report or metric is a new
// way to ask for data, so it needs the same answer as asking directly. These
// tests come at it from both directions: a caller who lacks the permission gets
// nothing, and a caller in another tenant gets nothing.

var asOf = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

// Every canned report refuses a caller without the permission it declares —
// checked at the single Run entry point, so a new report cannot forget it.
func TestReportsRefuseCallerWithoutPermission(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 50000)

			// A principal with no grants at all.
			blind := reportingActor(t, db, b.tenant, map[string][]string{})
			for _, d := range Reports() {
				if _, err := Run(blind, db, b.tenant, d.Name, Scope{Currency: "EUR", AsOf: asOf}); err == nil {
					t.Errorf("report %q rendered for a caller with no permissions (INV-T1)", d.Name)
				}
			}
		})
	}
}

// The gate is per-report, not global: reading the ledger does not entitle you to
// AR, and vice versa. A single coarse "reports" permission would be exactly the
// leak docs/21 warns about.
func TestReportPermissionsAreObjectSpecific(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 50000)

			// Ledger-only: statements yes, AR aging no.
			ledgerOnly := reportingActor(t, db, b.tenant, map[string][]string{
				ledger.ObjectJournalEntry: {"read"},
			})
			for _, name := range []string{"trial_balance", "profit_and_loss", "balance_sheet"} {
				if _, err := Run(ledgerOnly, db, b.tenant, name, Scope{Currency: "EUR", AsOf: asOf}); err != nil {
					t.Errorf("%s refused a caller who can read journal entries: %v", name, err)
				}
			}
			if _, err := Run(ledgerOnly, db, b.tenant, "ar_aging", Scope{Currency: "EUR", AsOf: asOf}); err == nil {
				t.Error("AR aging rendered for a caller who cannot read invoices (INV-T1)")
			}

			// Invoice-only: AR aging yes, statements no.
			invoiceOnly := reportingActor(t, db, b.tenant, map[string][]string{
				invoicing.ObjectInvoice: {"read"},
			})
			if _, err := Run(invoiceOnly, db, b.tenant, "ar_aging", Scope{Currency: "EUR", AsOf: asOf}); err != nil {
				t.Errorf("AR aging refused a caller who can read invoices: %v", err)
			}
			if _, err := Run(invoiceOnly, db, b.tenant, "trial_balance", Scope{Currency: "EUR", AsOf: asOf}); err == nil {
				t.Error("trial balance rendered for a caller who cannot read journal entries (INV-T1)")
			}
		})
	}
}

// A metric must not become a side channel around the report gate: asking for
// ar_outstanding is asking about invoices.
func TestMetricsRefuseCallerWithoutPermission(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 50000)

			blind := reportingActor(t, db, b.tenant, map[string][]string{})
			for _, m := range Metrics() {
				if _, err := Evaluate(blind, db, b.tenant, m.Name, Scope{Currency: "EUR", AsOf: asOf}); err == nil {
					t.Errorf("metric %q evaluated for a caller with no permissions (INV-T1)", m.Name)
				}
			}

			// The catalog is not secret — knowing a metric exists is not knowing
			// its value — but EvaluateAll must return nothing for this caller.
			values, err := EvaluateAll(blind, db, b.tenant, Scope{Currency: "EUR", AsOf: asOf})
			if err != nil {
				t.Fatalf("EvaluateAll: %v", err)
			}
			if len(values) != 0 {
				t.Errorf("EvaluateAll returned %d values for a caller with no permissions: %v", len(values), values)
			}
		})
	}
}

// EvaluateAll returns exactly the subset the caller may see — it neither leaks
// the rest nor fails the whole request because one tile is off-limits.
func TestEvaluateAllReturnsOnlyPermittedMetrics(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 50000)

			invoiceOnly := reportingActor(t, db, b.tenant, map[string][]string{
				invoicing.ObjectInvoice: {"read"},
			})
			values, err := EvaluateAll(invoiceOnly, db, b.tenant, Scope{Currency: "EUR", AsOf: asOf})
			if err != nil {
				t.Fatalf("EvaluateAll: %v", err)
			}
			if len(values) == 0 {
				t.Fatal("a caller who can read invoices got no metrics at all")
			}
			seen := map[string]bool{}
			for _, v := range values {
				seen[v.Metric] = true
			}
			for _, m := range Metrics() {
				if m.Object == invoicing.ObjectInvoice && !seen[m.Name] {
					t.Errorf("permitted metric %q was omitted", m.Name)
				}
				if m.Object == ledger.ObjectJournalEntry && seen[m.Name] {
					t.Errorf("metric %q leaked to a caller who cannot read journal entries (INV-T1)", m.Name)
				}
			}
		})
	}
}

// INV-T1: reports computed in one tenant must never include another's rows,
// even for a caller who holds every permission in their own tenant.
func TestReportsAreTenantScoped(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			// Tenant A has real books.
			a := seedBooks(t, db)
			inv := a.postInvoice(t, db, "2026-01-05", 1, 100000)
			a.receive(t, db, inv.ID, 50000)

			// Tenant B is empty but fully privileged in its own tenant.
			bTenant := tenancy.ID(idgen.New())
			if err := tenancy.CreateTenant(a.ctx, db, bTenant, "other tenant"); err != nil {
				t.Fatalf("create tenant: %v", err)
			}
			bCtx := reportingActor(t, db, bTenant, writerGrants())

			for _, name := range []string{"trial_balance", "profit_and_loss", "balance_sheet", "ar_aging"} {
				rep, err := Run(bCtx, db, bTenant, name, Scope{Currency: "EUR", AsOf: asOf})
				if err != nil {
					t.Fatalf("%s in the empty tenant: %v", name, err)
				}
				if len(rep.Rows) != 0 {
					t.Errorf("%s in an empty tenant returned %d rows — another tenant's data leaked (INV-T1): %+v",
						name, len(rep.Rows), rep.Rows)
				}
				for _, total := range rep.Totals {
					if total.AmountMinor != 0 {
						t.Errorf("%s total %q = %d in an empty tenant, want 0 (INV-T1)",
							name, total.Key, total.AmountMinor)
					}
				}
			}

			for _, m := range Metrics() {
				v, err := Evaluate(bCtx, db, bTenant, m.Name, Scope{Currency: "EUR", AsOf: asOf})
				if err != nil {
					t.Fatalf("metric %s in the empty tenant: %v", m.Name, err)
				}
				if v.Value != 0 {
					t.Errorf("metric %s = %d in an empty tenant, want 0 (INV-T1)", m.Name, v.Value)
				}
			}
		})
	}
}

// An unauthenticated caller (no actor in context at all) gets nothing. Without
// this, a background job or a mistaken internal call would render a full set of
// books with no principal behind it (INV-T2).
func TestReportsRefuseAnonymousCaller(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			b.postInvoice(t, db, "2026-01-05", 1, 100000)

			anon := t.Context()
			for _, d := range Reports() {
				_, err := Run(anon, db, b.tenant, d.Name, Scope{Currency: "EUR", AsOf: asOf})
				if err == nil {
					t.Errorf("report %q rendered with no actor in context", d.Name)
				}
			}
			for _, m := range Metrics() {
				if _, err := Evaluate(anon, db, b.tenant, m.Name, Scope{Currency: "EUR", AsOf: asOf}); err == nil {
					t.Errorf("metric %q evaluated with no actor in context", m.Name)
				}
			}
		})
	}
}

// A refusal must be an authz error, not a generic failure — the API layer maps
// it to 403, and a 500 here would look like a bug rather than a decision.
func TestRefusalIsAnAuthzError(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			blind := reportingActor(t, db, b.tenant, map[string][]string{})

			_, err := Run(blind, db, b.tenant, "trial_balance", Scope{Currency: "EUR", AsOf: asOf})
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !isPermissionDenied(err) {
				t.Errorf("refusal = %v, want an authz.ErrPermissionDenied so the edge maps it to 403", err)
			}
		})
	}
}

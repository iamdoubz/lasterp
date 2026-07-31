//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// The demo seeder is a bulk write path, and INV-X5 is explicit that bulk paths
// get batching, not bypasses. These tests assert the seeded book is a real one:
// balanced, gapless, and reachable only through the ordinary pipeline.

func TestSeededBookHoldsTheLedgerInvariants(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seededTenant(t, db)
			ctx := e.actorCtx(t, fullGrants())

			// INV-F1 in aggregate: every entry balanced, so the whole book nets
			// to zero per currency.
			if err := ledger.EnsureBalances(ctx, db, e.tenant); err != nil {
				t.Fatalf("EnsureBalances: %v", err)
			}
			tb, err := ledger.ReadTrialBalance(ctx, db, e.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}
			totals := map[string]int64{}
			for _, byCurrency := range tb {
				for currency, net := range byCurrency {
					totals[currency] += net
				}
			}
			if len(totals) == 0 {
				t.Fatal("the seeded book has no balances at all")
			}
			for currency, net := range totals {
				if net != 0 {
					t.Errorf("INV-F1: seeded book does not balance in %s (net %d)", currency, net)
				}
			}

			// INV-F6: numbers are allocated at acceptance and gapless.
			invoices, err := invoicing.ListPosted(ctx, db, e.tenant)
			if err != nil {
				t.Fatalf("ListPosted: %v", err)
			}
			if len(invoices) < 4 {
				t.Fatalf("seeded %d posted invoices, want the full demo book", len(invoices))
			}
			numbers := make([]string, 0, len(invoices))
			for _, inv := range invoices {
				if inv.Number == "" {
					t.Errorf("posted invoice %s has no number", inv.ID)
				}
				if inv.GLEntryID == "" {
					t.Errorf("posted invoice %s has no journal entry — it did not go through the pipeline", inv.ID)
				}
				numbers = append(numbers, inv.Number)
			}
			sort.Strings(numbers)
			for i, number := range numbers {
				if want := invoiceNumber(i + 1); number != want {
					t.Errorf("invoice number %d = %q, want %q (gapless, INV-F6)", i+1, number, want)
				}
			}

			// The seeded periods exist and are ordered, which is what makes the
			// dashboard's comparison possible.
			periods, err := ledger.ListPeriods(ctx, db, e.tenant)
			if err != nil {
				t.Fatalf("ListPeriods: %v", err)
			}
			if len(periods) != 2 {
				t.Fatalf("seeded %d periods, want 2", len(periods))
			}
			if periods[0].StartDate >= periods[1].StartDate {
				t.Errorf("periods are not in chronological order: %v", periods)
			}
		})
	}
}

// invoiceNumber renders the document-number-series format the invoicing module
// allocates (INV-000001).
func invoiceNumber(n int) string {
	digits := []byte("000000")
	for i := len(digits) - 1; i >= 0 && n > 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return "INV-" + string(digits)
}

// Seeding twice would double someone's revenue, so the second run is refused.
func TestSeedDemoRefusesANonEmptyBook(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)

	email, _ := namedRoleUser(t, e, "second-admin", fullGrants())
	err := SeedDemo(context.Background(), db, DemoInput{
		Tenant: string(e.tenant), Email: email,
		Today: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrDemoNotEmpty) {
		t.Fatalf("second SeedDemo: err = %v, want ErrDemoNotEmpty", err)
	}
}

// The seeder writes as a real user through the real authorization, so a user
// without the grants cannot use it as a side door into the ledger.
func TestSeedDemoRequiresTheGrants(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)

	email, _ := namedRoleUser(t, e, "reader", map[string][]string{"Account": {"read"}})
	err := SeedDemo(context.Background(), db, DemoInput{
		Tenant: string(e.tenant), Email: email,
		Today: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("SeedDemo succeeded for a user with no write grants — the seeder is a privileged side door")
	}
}

// The demo book includes a German-speaking customer, so the localized document
// path (WP-1.7) is exercised by the data a fresh deployment actually ships with.
func TestSeededBookCarriesACounterpartyLanguage(t *testing.T) {
	db := sqliteBootDB(t)
	e := seededTenant(t, db)
	ctx := e.actorCtx(t, fullGrants())

	invoices, err := invoicing.ListPosted(ctx, db, e.tenant)
	if err != nil {
		t.Fatalf("ListPosted: %v", err)
	}
	var localized int
	for _, inv := range invoices {
		if inv.Locale == "de" {
			localized++
		}
	}
	if localized == 0 {
		t.Error("no seeded invoice carries a counterparty language")
	}
}

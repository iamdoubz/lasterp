//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

// seedTwoPeriods extends the shared fixture with a second fiscal period and
// posts invoices into both, which is the minimum shape a period comparison
// needs: one period's movement, and a different one to compare it against.
func seedTwoPeriods(t *testing.T, db *storage.DB) books {
	t.Helper()
	b := seedBooks(t, db)
	if _, err := ledger.CreatePeriod(b.ctx, db, b.tenant, "2026-02", "2026-02-01", "2026-02-28"); err != nil {
		t.Fatalf("CreatePeriod: %v", err)
	}
	b.postInto(t, db, "2026-01", "2026-01-10", 100000)
	b.postInto(t, db, "2026-02", "2026-02-10", 250000)
	b.postInto(t, db, "2026-02", "2026-02-20", 50000)
	return b
}

// postInto posts one invoice into a named period.
func (b books) postInto(t *testing.T, db *storage.DB, period, issueDate string, minor int64) invoicing.Invoice {
	t.Helper()
	draft, err := invoicing.CreateDraft(b.ctx, db, b.tenant, invoicing.DraftInput{
		ContactID: b.contactID, Currency: "EUR", IssueDate: issueDate,
		ARAccount: b.arAccount, TaxAccount: b.taxAccount,
		Lines: []invoicing.Line{{
			Description: "Consulting", Quantity: 1, UnitPriceMinor: minor,
			RevenueAccount: b.revAccount, TaxJurisdiction: "DE", TaxCategory: tax.CategoryStandard,
		}},
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	inv, err := invoicing.PostInvoice(b.ctx, db, b.tenant, draft.ID, period)
	if err != nil {
		t.Fatalf("PostInvoice(%s): %v", period, err)
	}
	return inv
}

// INV-E5 — "projections are pure functions of the log: rebuild(events) ≡ current
// projection". WP-1.8 adds a *second* way to answer "what is the balance": a
// period-filtered fold, next to the materialized projection every report reads.
// Two answers to one question have to agree, or a dashboard tile and the balance
// sheet start arguing in front of a customer.
func TestUnfilteredPeriodFoldEqualsTheProjection(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := seedTwoPeriods(t, db)

			if err := ledger.EnsureBalances(f.ctx, db, f.tenant); err != nil {
				t.Fatalf("EnsureBalances: %v", err)
			}
			projection, err := ledger.ReadTrialBalance(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}

			folded, err := ledger.BalancesForPeriods(f.ctx, db, f.tenant, nil)
			if err != nil {
				t.Fatalf("BalancesForPeriods(nil): %v", err)
			}
			assertSameBalances(t, projection, folded)
		})
	}
}

// A stock balance at the last period must also equal the projection: "as at the
// end of the newest period" and "everything that ever happened" are the same
// statement about a book with no future entries.
func TestStockAtLatestPeriodEqualsTheProjection(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := seedTwoPeriods(t, db)

			if err := ledger.EnsureBalances(f.ctx, db, f.tenant); err != nil {
				t.Fatalf("EnsureBalances: %v", err)
			}
			projection, err := ledger.ReadTrialBalance(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}

			periods, err := ledger.ListPeriods(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ListPeriods: %v", err)
			}
			latest := periods[len(periods)-1]
			include, err := periodFilter(periods, latest.Code, GrainStock)
			if err != nil {
				t.Fatalf("periodFilter: %v", err)
			}
			stock, err := ledger.BalancesForPeriods(f.ctx, db, f.tenant, include)
			if err != nil {
				t.Fatalf("BalancesForPeriods(stock): %v", err)
			}
			assertSameBalances(t, projection, stock)
		})
	}
}

// Flow and stock must not be the same number, or the grain distinction is
// decoration: the flows of every period must sum to the closing stock.
func TestFlowsSumToStock(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := seedTwoPeriods(t, db)

			periods, err := ledger.ListPeriods(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ListPeriods: %v", err)
			}
			if len(periods) < 2 {
				t.Fatalf("fixture seeded %d periods, want at least 2", len(periods))
			}

			summed := ledger.TrialBalance{}
			for _, p := range periods {
				include, err := periodFilter(periods, p.Code, GrainFlow)
				if err != nil {
					t.Fatalf("periodFilter(%s): %v", p.Code, err)
				}
				flow, err := ledger.BalancesForPeriods(f.ctx, db, f.tenant, include)
				if err != nil {
					t.Fatalf("BalancesForPeriods(%s): %v", p.Code, err)
				}
				for account, byCurrency := range flow {
					for currency, net := range byCurrency {
						if summed[account] == nil {
							summed[account] = map[string]int64{}
						}
						summed[account][currency] += net
					}
				}
			}

			latest := periods[len(periods)-1]
			include, err := periodFilter(periods, latest.Code, GrainStock)
			if err != nil {
				t.Fatalf("periodFilter: %v", err)
			}
			stock, err := ledger.BalancesForPeriods(f.ctx, db, f.tenant, include)
			if err != nil {
				t.Fatalf("BalancesForPeriods(stock): %v", err)
			}
			assertSameBalances(t, stock, summed)

			// …and an earlier period's flow must be a strict subset of the
			// closing stock, or the filter is not filtering at all.
			firstInclude, err := periodFilter(periods, periods[0].Code, GrainFlow)
			if err != nil {
				t.Fatalf("periodFilter: %v", err)
			}
			first, err := ledger.BalancesForPeriods(f.ctx, db, f.tenant, firstInclude)
			if err != nil {
				t.Fatalf("BalancesForPeriods(first): %v", err)
			}
			if sameBalances(first, stock) {
				t.Error("the first period's movement equals the closing balance — the period filter is not applied")
			}
		})
	}
}

func assertSameBalances(t *testing.T, want, got ledger.TrialBalance) {
	t.Helper()
	for account, byCurrency := range want {
		for currency, net := range byCurrency {
			if got[account][currency] != net {
				t.Errorf("account %s/%s = %d, want %d", account, currency, got[account][currency], net)
			}
		}
	}
	for account, byCurrency := range got {
		for currency, net := range byCurrency {
			if want[account][currency] != net {
				t.Errorf("unexpected balance for %s/%s = %d", account, currency, net)
			}
		}
	}
}

func sameBalances(a, b ledger.TrialBalance) bool {
	count := func(tb ledger.TrialBalance) int {
		n := 0
		for _, byCurrency := range tb {
			n += len(byCurrency)
		}
		return n
	}
	if count(a) != count(b) {
		return false
	}
	for account, byCurrency := range a {
		for currency, net := range byCurrency {
			if b[account][currency] != net {
				return false
			}
		}
	}
	return true
}

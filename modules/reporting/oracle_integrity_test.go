//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// WP-1.6 AC: "report/metric totals reconcile with event-fold oracle."
//
// That is INV-E5 restated — projections are pure functions of the log, so
// rebuild(events) ≡ projection. Every report here is computed twice: once from
// the incrementally-maintained ledger_balances projection (the path production
// reads), and once from a fold of the raw event log (the oracle). They must
// agree exactly.
//
// The comparison is only meaningful because the two are genuinely different code
// paths: EnsureBalances applies deltas after a cursor, while FoldTrialBalance
// replays the whole log from nothing. Before WP-1.6 the projection was written
// only by a full re-fold, so comparing them would have compared the fold to
// itself.

// oracleData builds report Data straight from the event log, bypassing the
// projection entirely.
func oracleData(t *testing.T, db *storage.DB, tenant tenancy.ID, ctx context.Context, currency string) Data {
	t.Helper()
	var all []eventstore.Event
	var cursor int64
	for {
		batch, err := eventstore.ReadFeed(ctx, db, tenant, cursor, 1000)
		if err != nil {
			t.Fatalf("ReadFeed: %v", err)
		}
		all = append(all, batch...)
		if len(batch) < 1000 {
			break
		}
		cursor = batch[len(batch)-1].ID
	}
	folded, err := ledger.FoldTrialBalance(all)
	if err != nil {
		t.Fatalf("FoldTrialBalance: %v", err)
	}
	accounts, err := loadAccounts(ctx, db, tenant)
	if err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}
	return Data{Accounts: accounts, Balances: folded, Currency: currency}
}

func assertReportsEqual(t *testing.T, name string, got, want Report) {
	t.Helper()
	if len(got.Rows) != len(want.Rows) {
		t.Fatalf("%s: projection has %d rows, oracle has %d", name, len(got.Rows), len(want.Rows))
	}
	for i := range got.Rows {
		if got.Rows[i].Key != want.Rows[i].Key || got.Rows[i].AmountMinor != want.Rows[i].AmountMinor {
			t.Errorf("%s row %d: projection %s=%d, oracle %s=%d", name, i,
				got.Rows[i].Key, got.Rows[i].AmountMinor, want.Rows[i].Key, want.Rows[i].AmountMinor)
		}
	}
	if len(got.Totals) != len(want.Totals) {
		t.Fatalf("%s: projection has %d totals, oracle has %d", name, len(got.Totals), len(want.Totals))
	}
	for i := range got.Totals {
		if got.Totals[i].Key != want.Totals[i].Key || got.Totals[i].AmountMinor != want.Totals[i].AmountMinor {
			t.Errorf("%s total %q: projection %d, oracle %d", name,
				got.Totals[i].Key, got.Totals[i].AmountMinor, want.Totals[i].AmountMinor)
		}
	}
}

// TestReportsReconcileWithEventFoldOracle is the AC.
func TestReportsReconcileWithEventFoldOracle(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)

			// Build a non-trivial set of books: several invoices, partial and
			// full settlement, and a reversal, so the fold has real work to do.
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			inv2 := b.postInvoice(t, db, "2026-01-10", 2, 25000)
			inv3 := b.postInvoice(t, db, "2026-01-15", 3, 10000)
			b.receive(t, db, inv1.ID, 50000)
			b.receive(t, db, inv2.ID, inv2.GrossMinor)
			b.receive(t, db, inv3.ID, 1)

			// A reversal exercises the fold's handling of compensating entries.
			if _, err := ledger.Reverse(b.ctx, db, b.tenant, inv3.GLEntryID, "rev-"+inv3.ID); err != nil {
				t.Fatalf("Reverse: %v", err)
			}

			projection, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			oracle := oracleData(t, db, b.tenant, b.ctx, "EUR")

			assertReportsEqual(t, "trial_balance", TrialBalance(projection), TrialBalance(oracle))
			assertReportsEqual(t, "profit_and_loss", ProfitAndLoss(projection), ProfitAndLoss(oracle))
			assertReportsEqual(t, "balance_sheet", BalanceSheet(projection), BalanceSheet(oracle))

			if got, want := NetIncome(projection), NetIncome(oracle); got != want {
				t.Errorf("net income: projection %d, oracle %d", got, want)
			}

			// And the underlying balances agree account by account, so a future
			// report cannot inherit a discrepancy this suite did not look at.
			for account, byCurrency := range oracle.Balances {
				for currency, want := range byCurrency {
					if got := projection.Balances[account][currency]; got != want {
						t.Errorf("balance %s/%s: projection %d, oracle %d", account, currency, got, want)
					}
				}
			}
			for account, byCurrency := range projection.Balances {
				for currency, got := range byCurrency {
					if want := oracle.Balances[account][currency]; got != want {
						t.Errorf("balance %s/%s exists in the projection (%d) but not the oracle (%d)",
							account, currency, got, want)
					}
				}
			}
		})
	}
}

// The projection must converge to the same answer whether it was built
// incrementally over many reads or rebuilt from scratch in one pass — the
// operational form of INV-E5.
func TestIncrementalCatchUpMatchesFullRebuild(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)

			// Interleave posting with reads, so the projection catches up in
			// several small increments rather than one big one.
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			if _, err := Load(b.ctx, db, b.tenant, "EUR"); err != nil {
				t.Fatalf("Load: %v", err)
			}
			b.receive(t, db, inv1.ID, 30000)
			if _, err := Load(b.ctx, db, b.tenant, "EUR"); err != nil {
				t.Fatalf("Load: %v", err)
			}
			inv2 := b.postInvoice(t, db, "2026-01-11", 4, 5000)
			b.receive(t, db, inv2.ID, 1000)

			incremental, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			// Now throw the projection away and rebuild it in one pass.
			if err := ledger.RebuildBalances(b.ctx, db, b.tenant); err != nil {
				t.Fatalf("RebuildBalances: %v", err)
			}
			rebuilt, err := ledger.ReadTrialBalance(b.ctx, db, b.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}

			for account, byCurrency := range rebuilt {
				for currency, want := range byCurrency {
					if got := incremental.Balances[account][currency]; got != want {
						t.Errorf("account %s/%s: incremental %d, full rebuild %d (INV-E5)",
							account, currency, got, want)
					}
				}
			}
			if len(incremental.Balances) != len(rebuilt) {
				t.Errorf("incremental projection has %d accounts, rebuild has %d",
					len(incremental.Balances), len(rebuilt))
			}
		})
	}
}

// Catching up twice with no new events in between must change nothing.
func TestEnsureBalancesIsIdempotent(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 40000)

			first, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for i := 0; i < 5; i++ {
				if err := ledger.EnsureBalances(b.ctx, db, b.tenant); err != nil {
					t.Fatalf("EnsureBalances: %v", err)
				}
			}
			again, err := ledger.ReadTrialBalance(b.ctx, db, b.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}
			for account, byCurrency := range first.Balances {
				for currency, want := range byCurrency {
					if got := again[account][currency]; got != want {
						t.Errorf("account %s/%s drifted from %d to %d across repeated catch-ups",
							account, currency, want, got)
					}
				}
			}
		})
	}
}

// The trial balance sums to zero per currency, because every entry balances
// (INV-F1). If this ever fails, either the ledger or the projection is broken —
// and the report is how anyone would notice.
func TestTrialBalanceSumsToZero(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 3, 33333)
			b.receive(t, db, inv.ID, 12345)

			d, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var sum int64
			for _, byCurrency := range d.Balances {
				sum += byCurrency["EUR"]
			}
			if sum != 0 {
				t.Errorf("Σ net_minor across accounts = %d, want 0 (INV-F1)", sum)
			}
		})
	}
}

// A well-formed tenant has nothing unclassified — the bucket must not become
// normal furniture (WP-1.6-decisions.md §5).
func TestWellFormedTenantHasNothingUnclassified(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.receive(t, db, inv.ID, 50000)

			d, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := Unclassified(d); got != 0 {
				t.Errorf("unclassified total = %d, want 0 for a well-formed chart of accounts", got)
			}
			for _, row := range BalanceSheet(d).Totals {
				if row.Key == TypeUnclassified {
					t.Error("balance sheet rendered an unclassified line for well-formed books")
				}
			}
		})
	}
}

// The balance sheet balances against real books, not just the unit fixture.
func TestBalanceSheetBalancesAgainstRealBooks(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			inv2 := b.postInvoice(t, db, "2026-01-09", 7, 1234)
			b.receive(t, db, inv1.ID, 60000)
			b.receive(t, db, inv2.ID, 100)

			d, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			totals := map[string]int64{}
			for _, row := range BalanceSheet(d).Totals {
				totals[row.Key] = row.AmountMinor
			}
			if totals["assets"] != totals["liabilities"]+totals["equity"] {
				t.Errorf("assets %d != liabilities %d + equity %d",
					totals["assets"], totals["liabilities"], totals["equity"])
			}
		})
	}
}

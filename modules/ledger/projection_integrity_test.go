//go:build integrity

// Trial-balance projection suite (INV-E5: projections are a pure function of
// the log). Under randomized posting, the materialized ledger_balances
// projection must equal both an independent fold of the events and the balances
// the test itself accumulated from its inputs — triangulated three ways — and
// the whole trial balance must net to zero (a ledger-wide echo of INV-F1). Runs
// on Postgres AND SQLite.
package ledger

import (
	"context"
	"math/rand"
	"reflect"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func TestTrialBalanceProjectionMatchesFold(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			// A pool of accounts to post random entries between.
			pool := []string{f.cash, f.revenue}
			for _, spec := range []struct{ code, name, ty string }{
				{"5000", "Expense", AccountExpense},
				{"2000", "Payable", AccountLiability},
			} {
				a, err := CreateAccount(f.ctx, db, f.tenant, spec.code, spec.name, spec.ty, "", "")
				if err != nil {
					t.Fatalf("CreateAccount %s: %v", spec.name, err)
				}
				pool = append(pool, a["id"].(string))
			}

			rng := rand.New(rand.NewSource(0x1ED6E5))
			expected := TrialBalance{}
			const iterations = 150
			for i := 0; i < iterations; i++ {
				di := rng.Intn(len(pool))
				ci := rng.Intn(len(pool))
				if ci == di {
					ci = (ci + 1) % len(pool)
				}
				amount := rng.Int63n(1_000_000) + 1 // 1..1e6 minor units
				cmd := PostCmd{
					Period: f.period, Currency: "USD", CommandID: idFor(i),
					Lines: []Line{{AccountID: pool[di], Debit: amount}, {AccountID: pool[ci], Credit: amount}},
				}
				if _, err := Post(f.ctx, db, f.tenant, cmd); err != nil {
					t.Fatalf("Post %d: %v", i, err)
				}
				expected.add(pool[di], "USD", amount, 0)
				expected.add(pool[ci], "USD", 0, amount)
			}

			// Rebuild the materialized projection and read it back.
			if err := RebuildBalances(f.ctx, db, f.tenant); err != nil {
				t.Fatalf("RebuildBalances: %v", err)
			}
			projection, err := ReadTrialBalance(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}

			// Projection == the balances the test accumulated from its inputs.
			if !reflect.DeepEqual(projection, expected) {
				t.Fatalf("projection != expected\n projection=%v\n expected=%v", projection, expected)
			}

			// Projection == an independent fold of the event log (INV-E5).
			events := readAllEvents(t, f.ctx, db, f.tenant)
			folded, err := FoldTrialBalance(events)
			if err != nil {
				t.Fatalf("FoldTrialBalance: %v", err)
			}
			if !reflect.DeepEqual(projection, folded) {
				t.Fatalf("projection != fold\n projection=%v\n fold=%v", projection, folded)
			}

			// The whole trial balance nets to zero per currency (INV-F1 across the ledger).
			totals := map[string]int64{}
			for _, byCurrency := range projection {
				for currency, net := range byCurrency {
					totals[currency] += net
				}
			}
			for currency, total := range totals {
				if total != 0 {
					t.Fatalf("trial balance for %s totals %d, want 0", currency, total)
				}
			}
		})
	}
}

func idFor(i int) string {
	return "proj-cmd-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

func readAllEvents(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID) []eventstore.Event {
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
	return all
}

//go:build integrity

// Ledger posting integrity suite (joins the Integrity Gauntlet, docs/19). Runs
// on Postgres AND SQLite. Proves the WP-1.2 PR-A invariants at the posting
// pipeline choke point + append-only storage:
//
//	INV-F1 an entry that does not balance is rejected;
//	INV-F2 a posted entry is immutable (raw UPDATE/DELETE rejected) and a
//	       correction is a new reversing entry that leaves the original intact;
//	INV-F3 posting into a closed period is rejected; close is monotonic;
//	INV-T2 posting requires the JournalEntry "post" permission.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

type fixture struct {
	ctx      context.Context
	tenant   tenancy.ID
	cash     string
	revenue  string
	period   string
	periodID string
}

func setup(t *testing.T, db *storage.DB) fixture {
	t.Helper()
	tenant := mustCreateTenant(t, db)
	if err := Register(context.Background(), db); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := fullActor(t, db, tenant)

	cash, err := CreateAccount(ctx, db, tenant, "1000", "Cash", AccountAsset, "", "")
	if err != nil {
		t.Fatalf("CreateAccount cash: %v", err)
	}
	revenue, err := CreateAccount(ctx, db, tenant, "4000", "Revenue", AccountIncome, "", "")
	if err != nil {
		t.Fatalf("CreateAccount revenue: %v", err)
	}
	period, err := CreatePeriod(ctx, db, tenant, "2026-01", "2026-01-01", "2026-01-31")
	if err != nil {
		t.Fatalf("CreatePeriod: %v", err)
	}
	return fixture{
		ctx: ctx, tenant: tenant,
		cash: cash["id"].(string), revenue: revenue["id"].(string),
		period: "2026-01", periodID: period["id"].(string),
	}
}

// balanced returns a DR cash / CR revenue entry for amount minor units.
func (f fixture) balanced(amount int64, commandID string) PostCmd {
	return PostCmd{
		Period: f.period, Currency: "USD", CommandID: commandID,
		Lines: []Line{{AccountID: f.cash, Debit: amount}, {AccountID: f.revenue, Credit: amount}},
	}
}

func TestPostAndLoad(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			entry, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-1"))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			got, err := LoadEntry(f.ctx, db, f.tenant, entry.ID)
			if err != nil {
				t.Fatalf("LoadEntry: %v", err)
			}
			if got.Currency != "USD" || len(got.Lines) != 2 {
				t.Fatalf("loaded entry = %+v", got)
			}
			if got.Event.Type != EventPosted {
				t.Fatalf("event type = %q, want %q", got.Event.Type, EventPosted)
			}
		})
	}
}

// INV-F1: an unbalanced entry is rejected.
func TestPostUnbalancedRejected(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			cmd := PostCmd{Period: f.period, Currency: "USD", CommandID: "cmd-u",
				Lines: []Line{{AccountID: f.cash, Debit: 10000}, {AccountID: f.revenue, Credit: 9999}}}
			if _, err := Post(f.ctx, db, f.tenant, cmd); !errors.Is(err, ErrUnbalanced) {
				t.Fatalf("Post unbalanced: err = %v, want ErrUnbalanced", err)
			}
			// And nothing was written.
			assertEventCount(t, f.ctx, db, f.tenant, 0)
		})
	}
}

// INV-F3: posting into a closed period is rejected; close is monotonic.
func TestPostClosedPeriodRejected(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			if err := ClosePeriod(f.ctx, db, f.tenant, f.periodID); err != nil {
				t.Fatalf("ClosePeriod: %v", err)
			}
			if _, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-c")); !errors.Is(err, ErrClosedPeriod) {
				t.Fatalf("Post into closed period: err = %v, want ErrClosedPeriod", err)
			}
			// Monotonic: closing an already-closed period errors.
			if err := ClosePeriod(f.ctx, db, f.tenant, f.periodID); !errors.Is(err, ErrPeriodNotOpen) {
				t.Fatalf("re-close: err = %v, want ErrPeriodNotOpen", err)
			}
			// Reopen (privileged, audited) restores posting.
			if err := ReopenPeriod(f.ctx, db, f.tenant, f.periodID); err != nil {
				t.Fatalf("ReopenPeriod: %v", err)
			}
			if _, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-c2")); err != nil {
				t.Fatalf("Post after reopen: %v", err)
			}
		})
	}
}

func TestPostUnknownAccount(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			cmd := PostCmd{Period: f.period, Currency: "USD", CommandID: "cmd-a",
				Lines: []Line{{AccountID: "nope", Debit: 100}, {AccountID: f.revenue, Credit: 100}}}
			if _, err := Post(f.ctx, db, f.tenant, cmd); !errors.Is(err, ErrAccountNotFound) {
				t.Fatalf("Post unknown account: err = %v, want ErrAccountNotFound", err)
			}
		})
	}
}

// INV-T2: posting requires the "post" permission.
func TestPostRequiresPermission(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			// A reader without "post".
			readerCtx := ledgerActor(t, db, f.tenant, map[string][]string{ObjectJournalEntry: {"read"}})
			if _, err := Post(readerCtx, db, f.tenant, f.balanced(100, "cmd-p")); !errors.Is(err, authz.ErrPermissionDenied) {
				t.Fatalf("Post without permission: err = %v, want ErrPermissionDenied", err)
			}
		})
	}
}

// INV-F2: a posted entry is immutable — a raw UPDATE or DELETE on its event is
// rejected by the append-only trigger.
func TestPostedEntryImmutable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			if _, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-i")); err != nil {
				t.Fatalf("Post: %v", err)
			}
			err := tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, db.Rebind(`UPDATE events SET actor_id = 'tamper' WHERE tenant_id = ?`), string(f.tenant))
				return e
			})
			if err == nil {
				t.Fatal("UPDATE on events must be rejected (INV-F2/E1 append-only)")
			}
			err = tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, db.Rebind(`DELETE FROM events WHERE tenant_id = ?`), string(f.tenant))
				return e
			})
			if err == nil {
				t.Fatal("DELETE on events must be rejected (INV-F2/E1 append-only)")
			}
		})
	}
}

// INV-F2: a correction is a reversing entry; the original is untouched.
func TestReverseCreatesNegatedEntry(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			orig, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-o"))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			rev, err := Reverse(f.ctx, db, f.tenant, orig.ID, "cmd-rev")
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}
			if rev.ID == orig.ID {
				t.Fatal("reversal must be a new entry, not a mutation of the original")
			}
			if rev.ReversesEntryID != orig.ID {
				t.Fatalf("reversal reverses_entry_id = %q, want %q", rev.ReversesEntryID, orig.ID)
			}
			// Lines swapped: original DR cash / CR revenue → reversal CR cash / DR revenue.
			byAccount := map[string]Line{}
			for _, l := range rev.Lines {
				byAccount[l.AccountID] = l
			}
			if byAccount[f.cash].Credit != 10000 || byAccount[f.revenue].Debit != 10000 {
				t.Fatalf("reversal lines not negated: %+v", rev.Lines)
			}
			// Original still reads back unchanged.
			reloaded, err := LoadEntry(f.ctx, db, f.tenant, orig.ID)
			if err != nil {
				t.Fatalf("LoadEntry original: %v", err)
			}
			if reloaded.ReversesEntryID != "" || len(reloaded.Lines) != 2 {
				t.Fatalf("original entry changed: %+v", reloaded)
			}
		})
	}
}

// INV-E4: a replayed post (same command_id) yields the same event, not a duplicate.
func TestPostIdempotent(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			first, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-once"))
			if err != nil {
				t.Fatalf("first Post: %v", err)
			}
			second, err := Post(f.ctx, db, f.tenant, f.balanced(10000, "cmd-once"))
			if err != nil {
				t.Fatalf("replay Post: %v", err)
			}
			if first.Event.ID != second.Event.ID || first.ID != second.ID {
				t.Fatalf("replay produced a different entry: %s vs %s", first.ID, second.ID)
			}
			assertEventCount(t, f.ctx, db, f.tenant, 1)
		})
	}
}

func assertEventCount(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID, want int) {
	t.Helper()
	var n int
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(`SELECT COUNT(*) FROM events WHERE tenant_id = ?`), string(tenant)).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != want {
		t.Fatalf("event count = %d, want %d", n, want)
	}
}

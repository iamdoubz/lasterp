// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// TrialBalance maps account id → currency → net minor units (Σdebits −
// Σcredits; positive is a net debit). Because every entry balances (INV-F1),
// the net across all accounts is zero per currency.
type TrialBalance map[string]map[string]int64

// add accumulates a debit/credit onto (account, currency).
func (tb TrialBalance) add(account, currency string, debit, credit int64) {
	if tb[account] == nil {
		tb[account] = map[string]int64{}
	}
	tb[account][currency] += debit - credit
}

// FoldTrialBalance computes the trial balance directly from an event slice — a
// pure function of the log (INV-E5). It is the oracle the materialized
// projection is verified against, and the fold WP-1.6 reports build on.
func FoldTrialBalance(events []eventstore.Event) (TrialBalance, error) {
	tb := TrialBalance{}
	for _, ev := range events {
		if ev.Type != EventPosted {
			continue
		}
		var p entryPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, fmt.Errorf("ledger: fold decode event %d: %w", ev.ID, err)
		}
		for _, l := range p.Lines {
			tb.add(l.AccountID, p.Currency, l.Debit, l.Credit)
		}
	}
	return tb, nil
}

// readEventsSince pages the tenant's event feed from after.
func readEventsSince(ctx context.Context, db *storage.DB, tenant tenancy.ID, after int64) ([]eventstore.Event, error) {
	const page = 1000
	var all []eventstore.Event
	cursor := after
	for {
		batch, err := eventstore.ReadFeed(ctx, db, tenant, cursor, page)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < page {
			break
		}
		cursor = batch[len(batch)-1].ID
	}
	return all, nil
}

// RebuildBalances recomputes the ledger_balances projection for tenant from the
// full event log — the projection is derived state, rebuildable at any time
// (INV-E5). It replaces the tenant's rows and resets the cursor in one
// transaction.
func RebuildBalances(ctx context.Context, db *storage.DB, tenant tenancy.ID) error {
	all, err := readEventsSince(ctx, db, tenant, 0)
	if err != nil {
		return err
	}
	tb, err := FoldTrialBalance(all)
	if err != nil {
		return err
	}
	var high int64
	for _, ev := range all {
		if ev.ID > high {
			high = ev.ID
		}
	}

	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, db.Rebind(`DELETE FROM ledger_balances WHERE tenant_id = ?`), string(tenant)); err != nil {
			return err
		}
		for account, byCurrency := range tb {
			for currency, net := range byCurrency {
				if _, err := tx.ExecContext(ctx, db.Rebind(
					`INSERT INTO ledger_balances (tenant_id, account_id, currency, net_minor) VALUES (?, ?, ?, ?)`),
					string(tenant), account, currency, net); err != nil {
					return err
				}
			}
		}
		return setBalanceCursor(ctx, tx, db, tenant, high)
	})
}

// EnsureBalances brings the projection up to date by folding only the events
// appended since the stored cursor, then advancing it — all in one transaction.
//
// This is the path reads use. Posting deliberately does not maintain the
// projection: the Postgres write path goes through a SECURITY DEFINER pipeline
// function that owns the append (INV-F5), and hanging a second write off it
// would either widen the app role's grants or leave a window where a crash
// between the two makes the projection silently wrong. Catching up at read time
// is exact by construction — there is no staleness window to reason about.
//
// It is idempotent and safe to call concurrently: the fold applies only events
// strictly after the cursor, and the cursor advance is in the same transaction
// as the deltas it accounts for.
//
// ponytail: catch-up is O(new events) per read and the whole log on first call.
// If a tenant's feed ever outgrows that, move the catch-up to a background
// worker driven by the same cursor — the read path does not change.
func EnsureBalances(ctx context.Context, db *storage.DB, tenant tenancy.ID) error {
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		from, err := balanceCursor(ctx, tx, db, tenant)
		if err != nil {
			return err
		}
		// Read the new events inside the same transaction, so what is folded and
		// what the cursor claims cannot diverge.
		events, err := readFeedTx(ctx, tx, db, tenant, from)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		delta, err := FoldTrialBalance(events)
		if err != nil {
			return err
		}
		for account, byCurrency := range delta {
			for currency, net := range byCurrency {
				if net == 0 {
					continue
				}
				if err := applyBalanceDelta(ctx, tx, db, tenant, account, currency, net); err != nil {
					return err
				}
			}
		}
		var high int64
		for _, ev := range events {
			if ev.ID > high {
				high = ev.ID
			}
		}
		return setBalanceCursor(ctx, tx, db, tenant, high)
	})
}

// readFeedTx reads the tenant's events after cursor within tx.
func readFeedTx(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, after int64) ([]eventstore.Event, error) {
	rows, err := tx.QueryContext(ctx, db.Rebind(`
		SELECT id, stream_id, type, payload
		FROM events WHERE tenant_id = ? AND id > ?
		ORDER BY id`), string(tenant), after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []eventstore.Event
	for rows.Next() {
		var ev eventstore.Event
		var stream, typ string
		var payload []byte
		if err := rows.Scan(&ev.ID, &stream, &typ, &payload); err != nil {
			return nil, err
		}
		ev.StreamID = eventstore.StreamID(stream)
		ev.Type = eventstore.EventType(typ)
		ev.Payload = payload
		out = append(out, ev)
	}
	return out, rows.Err()
}

// applyBalanceDelta adds net to (account, currency), inserting the row if it is
// the account's first movement. Written as update-then-insert rather than an
// upsert because ON CONFLICT and INSERT OR REPLACE differ between the dialects
// (ADR-015 portability) and this stays one code path.
func applyBalanceDelta(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, account, currency string, net int64) error {
	res, err := tx.ExecContext(ctx, db.Rebind(`
		UPDATE ledger_balances SET net_minor = net_minor + ?
		WHERE tenant_id = ? AND account_id = ? AND currency = ?`),
		net, string(tenant), account, currency)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, db.Rebind(
		`INSERT INTO ledger_balances (tenant_id, account_id, currency, net_minor) VALUES (?, ?, ?, ?)`),
		string(tenant), account, currency, net)
	return err
}

func balanceCursor(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID) (int64, error) {
	var last int64
	err := tx.QueryRowContext(ctx, db.Rebind(
		`SELECT last_event_id FROM ledger_balance_cursor WHERE tenant_id = ?`), string(tenant)).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return last, err
}

func setBalanceCursor(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, last int64) error {
	res, err := tx.ExecContext(ctx, db.Rebind(
		`UPDATE ledger_balance_cursor SET last_event_id = ?, updated_at = ? WHERE tenant_id = ?`),
		last, time.Now().UTC(), string(tenant))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, db.Rebind(
		`INSERT INTO ledger_balance_cursor (tenant_id, last_event_id, updated_at) VALUES (?, ?, ?)`),
		string(tenant), last, time.Now().UTC())
	return err
}

// ReadTrialBalance reads the materialized ledger_balances projection.
func ReadTrialBalance(ctx context.Context, db *storage.DB, tenant tenancy.ID) (TrialBalance, error) {
	tb := TrialBalance{}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT account_id, currency, net_minor FROM ledger_balances WHERE tenant_id = ?`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var account, currency string
			var net int64
			if err := rows.Scan(&account, &currency, &net); err != nil {
				return err
			}
			if tb[account] == nil {
				tb[account] = map[string]int64{}
			}
			tb[account][currency] = net
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return tb, nil
}

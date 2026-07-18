// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

// RebuildBalances recomputes the ledger_balances projection for tenant from the
// full event log — the projection is derived state, rebuildable at any time
// (INV-E5). It replaces the tenant's rows in one transaction.
func RebuildBalances(ctx context.Context, db *storage.DB, tenant tenancy.ID) error {
	const page = 1000
	var all []eventstore.Event
	var cursor int64
	for {
		batch, err := eventstore.ReadFeed(ctx, db, tenant, cursor, page)
		if err != nil {
			return err
		}
		all = append(all, batch...)
		if len(batch) < page {
			break
		}
		cursor = batch[len(batch)-1].ID
	}

	tb, err := FoldTrialBalance(all)
	if err != nil {
		return err
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
		return nil
	})
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

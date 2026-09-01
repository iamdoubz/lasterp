// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// loadAccounts reads the tenant's chart of accounts. It queries the generated
// Account table directly rather than going through the CRUD engine because a
// report needs every account in one pass, and the CRUD List returns untyped
// records it would only have to re-decode.
//
// Archived accounts are included: an account archived today may still carry
// balances from entries posted before it was archived, and dropping it would
// unbalance the trial balance (INV-F1's Σ=0 property would fail for a reason
// that is a reporting bug, not a ledger one).
func loadAccounts(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Account, error) {
	table := metadata.TableName(ledger.ObjectAccount)
	var out []Account
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT id, code, name, type, COALESCE(currency, '') FROM `+table+`
			 WHERE tenant_id = ? ORDER BY code`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []Account
		for rows.Next() {
			var a Account
			if err := rows.Scan(&a.ID, &a.Code, &a.Name, &a.Type, &a.Currency); err != nil {
				return err
			}
			list = append(list, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reporting: load accounts: %w", err)
	}
	return out, nil
}

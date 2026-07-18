// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Account types set the account's normal balance and its P&L/balance-sheet
// classification (docs/03). Closed set — the ledger validates against it.
const (
	AccountAsset     = "asset"
	AccountLiability = "liability"
	AccountEquity    = "equity"
	AccountIncome    = "income"
	AccountExpense   = "expense"
)

var validAccountTypes = map[string]bool{
	AccountAsset: true, AccountLiability: true, AccountEquity: true,
	AccountIncome: true, AccountExpense: true,
}

// ErrInvalidAccountType is returned by CreateAccount for a type outside the
// closed set.
var ErrInvalidAccountType = errors.New("ledger: invalid account type")

// ErrAccountNotFound is returned by the posting pipeline when a line references
// an account that does not exist (or is archived) for the tenant.
var ErrAccountNotFound = errors.New("ledger: account not found")

// CreateAccount adds a chart-of-accounts entry (CRUD object). type must be one
// of the closed AccountType set. parent (optional) links a tree; currency
// (optional) pins a single-currency account.
func CreateAccount(ctx context.Context, db *storage.DB, tenant tenancy.ID, code, name, accountType, parent, currency string) (metadata.Record, error) {
	if !validAccountTypes[accountType] {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAccountType, accountType)
	}
	crud, err := accountCRUD()
	if err != nil {
		return nil, err
	}
	rec := metadata.Record{"code": code, "name": name, "type": accountType}
	if parent != "" {
		rec["parent"] = parent
	}
	if currency != "" {
		rec["currency"] = currency
	}
	return crud.Create(ctx, db, tenant, rec)
}

// accountActive reports whether id names a live (non-archived) account for the
// tenant. It reads the generated table directly rather than through CRUD.Get so
// the internal integrity check doesn't demand the posting actor also hold
// Account "read" permission.
func accountActive(ctx context.Context, tx txQuerier, db *storage.DB, tenant tenancy.ID, id string) (bool, error) {
	var n int
	row := tx.QueryRowContext(ctx, db.Rebind(
		`SELECT COUNT(*) FROM `+metadata.TableName(ObjectAccount)+` WHERE tenant_id = ? AND id = ? AND archived_at IS NULL`),
		string(tenant), id)
	if err := row.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

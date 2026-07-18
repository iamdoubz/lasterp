// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// allocateNumber returns the next gapless document number for (tenant, series)
// and advances the counter, within tx (INV-F6). It must run inside the
// transaction that commits the document's acceptance, so a rolled-back
// acceptance consumes no number and concurrent acceptances serialize on the
// counter row (no dup, no gap).
//
// The counter is created lazily starting at 1. The read-then-write is safe
// under concurrency because it runs inside the caller's serialized tx: on
// Postgres the UPDATE takes a row lock (a second tx blocks until commit); on
// SQLite the whole write transaction is serialized by the database lock.
func allocateNumber(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, series string) (int64, error) {
	var next int64
	row := tx.QueryRowContext(ctx, db.Rebind(
		`SELECT next_value FROM document_number_series WHERE tenant_id = ? AND series = ?`),
		string(tenant), series)
	err := row.Scan(&next)
	now := time.Now().UTC()
	switch {
	case err == sql.ErrNoRows:
		// First document in this series: seed the counter at 2 (this doc takes 1).
		next = 1
		if _, err := tx.ExecContext(ctx, db.Rebind(
			`INSERT INTO document_number_series (tenant_id, series, next_value, updated_at) VALUES (?, ?, ?, ?)`),
			string(tenant), series, next+1, now); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if _, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE document_number_series SET next_value = next_value + 1, updated_at = ? WHERE tenant_id = ? AND series = ?`),
			now, string(tenant), series); err != nil {
			return 0, err
		}
	}
	return next, nil
}

// formatInvoiceNumber renders an allocated sequence value as a document number.
// ponytail: fixed "INV-000001" width; per-tenant prefix/format policy is a
// customization field to add when a tenant asks, not before.
func formatInvoiceNumber(seq int64) string {
	return fmt.Sprintf("INV-%06d", seq)
}

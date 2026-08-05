// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Rotate re-wraps every secret in a tenant that is still sealed under an older
// key-encryption key, and returns how many it moved.
//
// It unwraps each row's data key with the key that sealed it and re-wraps it
// with the source's current key. **The ciphertext is not touched** — that is
// the whole point of a per-secret data key (WP-3.0-decisions.md §1), and the
// acceptance test asserts the column is byte-identical afterwards rather than
// taking this comment's word for it.
//
// Resumable by construction: the work list is "key_id is not the current one",
// so a crash halfway leaves the remainder selectable on the next run. Rows are
// re-wrapped one transaction at a time for the same reason — a single
// transaction over a whole tenant would make a partial run all-or-nothing for
// no benefit, since each row is independent.
//
// The old key must still be in the key source. Removing a retired key before
// rotation has drained it is what makes a secret unrecoverable, which is why
// Unwrap says so by name rather than reporting a decryption failure.
func Rotate(ctx context.Context, db *storage.DB, src KeySource, tenant tenancy.ID, actor string) (int, error) {
	if src == nil {
		return 0, ErrNoKeySource
	}
	if tenant == "" || actor == "" {
		return 0, fmt.Errorf("secrets: tenant and actor are required")
	}

	var stale []string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT name FROM secrets WHERE tenant_id = ? AND key_id <> ? ORDER BY name`),
			string(tenant), src.KeyID())
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			stale = append(stale, name)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("secrets: rotate: list: %w", err)
	}

	rotated := 0
	for _, name := range stale {
		if err := rewrap(ctx, db, src, tenant, name, actor); err != nil {
			return rotated, fmt.Errorf("secrets: rotate %q: %w", name, err)
		}
		rotated++
	}
	return rotated, nil
}

func rewrap(ctx context.Context, db *storage.DB, src KeySource, tenant tenancy.ID, name, actor string) error {
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var keyID, wrappedB64 string
		if err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT key_id, wrapped_dek FROM secrets WHERE tenant_id = ? AND name = ?`),
			string(tenant), name).Scan(&keyID, &wrappedB64); err != nil {
			return err
		}
		if keyID == src.KeyID() {
			return nil // rotated by someone else between the list and here
		}
		wrapped, err := decode(wrappedB64)
		if err != nil {
			return err
		}
		aad := dekAAD(tenant, name)
		dek, err := src.Unwrap(wrapped, keyID, aad)
		if err != nil {
			return err
		}
		rewrapped, newKeyID, err := src.Wrap(dek, aad)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE secrets SET key_id = ?, wrapped_dek = ? WHERE tenant_id = ? AND name = ?`),
			newKeyID, encode(rewrapped), string(tenant), name); err != nil {
			return err
		}
		// A re-wrap is still an UPDATE on a tenant's row, so it is attributable
		// like any other (INV-T4). The audit row records which key the secret
		// moved to, which is what an operator checking a rotation completed
		// actually wants to see.
		return recordAudit(ctx, tx, db, tenant, name, "rotate", actor,
			map[string]any{"from_key_id": keyID, "to_key_id": newKeyID})
	})
}

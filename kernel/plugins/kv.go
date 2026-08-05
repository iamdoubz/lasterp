// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// kv limits. Small on purpose: this is a place for a cursor or a dedupe key,
// not a database a plugin brought with it.
const (
	MaxKVKeyBytes   = 256
	MaxKVValueBytes = 64 << 10
	MaxKVEntries    = 10000
)

// ErrKVFull is returned when a plugin has filled its allowance.
var ErrKVFull = errors.New("plugins: kv store is full for this plugin")

// kvGet reads one plugin-scoped value. Scoping is by construction — the
// primary key is (tenant, plugin, key) and every statement here binds both, so
// there is no query shape in which one plugin can address another's keys, or
// one tenant another's (INV-T1).
func kvGet(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin, key string) (string, bool, error) {
	var value string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT value FROM plugin_kv WHERE tenant_id = ? AND plugin_id = ? AND key = ?`),
			string(tenant), plugin, key).Scan(&value)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("plugins: kv get: %w", err)
	}
	return value, true, nil
}

// kvSet writes one plugin-scoped value, or deletes it when value is empty.
func kvSet(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin, key, value string) error {
	if key == "" || len(key) > MaxKVKeyBytes {
		return fmt.Errorf("plugins: kv key must be 1-%d bytes", MaxKVKeyBytes)
	}
	if len(value) > MaxKVValueBytes {
		return fmt.Errorf("plugins: kv value is %d bytes, over the %d-byte limit", len(value), MaxKVValueBytes)
	}
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if value == "" {
			_, err := tx.ExecContext(ctx, db.Rebind(
				`DELETE FROM plugin_kv WHERE tenant_id = ? AND plugin_id = ? AND key = ?`),
				string(tenant), plugin, key)
			return err
		}
		// The entry cap is checked before an insert that would add one, not on
		// every write: overwriting an existing key is always allowed, so a
		// plugin at its limit can still update what it already holds.
		var count int
		if err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT COUNT(*) FROM plugin_kv WHERE tenant_id = ? AND plugin_id = ?`),
			string(tenant), plugin).Scan(&count); err != nil {
			return err
		}
		if count >= MaxKVEntries {
			var exists int
			if err := tx.QueryRowContext(ctx, db.Rebind(
				`SELECT COUNT(*) FROM plugin_kv WHERE tenant_id = ? AND plugin_id = ? AND key = ?`),
				string(tenant), plugin, key).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrKVFull
			}
		}
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO plugin_kv (tenant_id, plugin_id, key, value, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, plugin_id, key) DO UPDATE SET
				value = excluded.value, updated_at = excluded.updated_at`),
			string(tenant), plugin, key, value, time.Now().UTC())
		return err
	})
}

// hostKVGet and hostKVSet are the host functions.
//
// They are unconditional, like log: plugin-scoped storage is not authority over
// anything the tenant owns — a plugin can only ever read back what it itself
// wrote — and at-least-once delivery makes idempotency the hook author's job,
// which needs somewhere to keep a dedupe key.
func hostKVGet(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	key, _ := req["key"].(string)
	value, found, err := kvGet(ctx, inv.host.DB, inv.tenant, inv.plugin.ID, key)
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": found, "value": value}, nil
}

func hostKVSet(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	key, _ := req["key"].(string)
	value, _ := req["value"].(string)
	if err := kvSet(ctx, inv.host.DB, inv.tenant, inv.plugin.ID, key, value); err != nil {
		return nil, err
	}
	return map[string]any{"stored": true}, nil
}

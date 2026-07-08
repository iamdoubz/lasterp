// SPDX-License-Identifier: AGPL-3.0-only

// Package tenancy carries the authenticated tenant through a request and,
// on Postgres, sets the session variable RLS policies key off (ADR-005,
// INV-T1). On SQLite (solo mode) there is exactly one tenant per replica
// and no RLS engine to configure, so SetContext is a no-op there — the
// repository layer is expected to filter by tenant_id itself regardless of
// dialect; RLS is a backstop, not the only guard.
package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// ID identifies a tenant. It is never derived from request parameters —
// only from the authenticated session (kernel/identity).
type ID string

type contextKey struct{}

// SetContext binds tenant to ctx and, on Postgres, sets app.tenant_id for
// the given transaction so RLS policies can enforce isolation. tx must be
// a transaction (SET LOCAL/set_config(..., true) is transaction-scoped) —
// never call this against a pooled *storage.DB directly.
func SetContext(ctx context.Context, tx *sql.Tx, dialect storage.Dialect, tenant ID) (context.Context, error) {
	ctx = context.WithValue(ctx, contextKey{}, tenant)
	if dialect != storage.Postgres {
		return ctx, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, string(tenant)); err != nil {
		return ctx, fmt.Errorf("tenancy: set app.tenant_id: %w", err)
	}
	return ctx, nil
}

// FromContext returns the tenant bound by SetContext, if any.
func FromContext(ctx context.Context) (ID, bool) {
	tenant, ok := ctx.Value(contextKey{}).(ID)
	return tenant, ok
}

// CreateTenant provisions a new tenant row. tenants is the one table that
// is not itself tenant-scoped (ADR-005) — every other table's tenant_id
// foreign-keys into this one.
func CreateTenant(ctx context.Context, db *storage.DB, id ID, name string) error {
	if id == "" || name == "" {
		return fmt.Errorf("tenancy: id and name are required")
	}
	_, err := db.ExecContext(ctx, db.Rebind(`INSERT INTO tenants (id, name, created_at) VALUES (?, ?, ?)`),
		string(id), name, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("tenancy: create tenant: %w", err)
	}
	return nil
}

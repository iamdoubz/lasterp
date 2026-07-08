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

// WithTenant runs fn inside a transaction with tenant bound via
// SetContext, committing on success and rolling back on error.
//
// This is the correct way to run any tenant-scoped query — every
// exported function in kernel/identity, kernel/authz, and
// kernel/eventstore goes through it. A bare statement issued straight
// against the pooled *storage.DB (db.ExecContext/QueryRowContext) grabs
// whatever connection is free from the pool with no tenant context set on
// it: RLS's USING clause doubles as WITH CHECK on Postgres, so with no
// context that's `tenant_id = NULL`, which matches nothing — every read
// silently returns zero rows and every write is rejected, for a
// non-superuser role. It only worked in early testing because the test
// harness connected as the cluster superuser, which always bypasses RLS
// regardless (see kernel/tenancy's own WP-0.3 RLS tests, which caught this
// exact class of false positive) — a real app role would have broken
// immediately.
//
// On SQLite, a transient storage.IsBusy (SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT,
// "database is locked") is retried against a wall-clock budget, not a
// fixed attempt count: busy_timeout reduces but does not eliminate this
// under real concurrent load from multiple goroutines/connections
// (kernel/eventstore's 1000-writer torture test hits it routinely without
// this retry), and SQLITE_BUSY_SNAPSHOT in particular can fail near-
// instantly rather than actually waiting out busy_timeout — so how many
// retries a given contention level needs varies with how fast the
// machine is (a fixed count of 50 passed locally but wasn't enough on a
// slower CI runner). Any other error, including the caller's own business
// errors (e.g. eventstore's ErrVersionConflict), propagates immediately
// without retrying.
func WithTenant(ctx context.Context, db *storage.DB, tenant ID, fn func(ctx context.Context, tx *sql.Tx) error) error {
	const busyRetryBudget = 30 * time.Second
	const maxBackoff = 200 * time.Millisecond

	deadline := time.Now().Add(busyRetryBudget)
	var err error
	for attempt := 0; ; attempt++ {
		err = withTenantOnce(ctx, db, tenant, fn)
		if err == nil || !storage.IsBusy(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tenancy: gave up after %s on SQLITE_BUSY: %w", busyRetryBudget, err)
		}
		backoff := time.Duration(attempt+1) * 5 * time.Millisecond
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		time.Sleep(backoff)
	}
}

func withTenantOnce(ctx context.Context, db *storage.DB, tenant ID, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("tenancy: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx, err = SetContext(ctx, tx, db.Dialect, tenant)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenancy: commit: %w", err)
	}
	return nil
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

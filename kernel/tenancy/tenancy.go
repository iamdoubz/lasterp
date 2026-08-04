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
	"math/rand/v2"
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
		time.Sleep(BusyBackoff(attempt))
	}
}

// BusyBackoff returns how long to wait before retrying a busy statement:
// uniformly random in (0, ceiling], where the ceiling grows with attempt.
//
// The randomness is the point, and the previous version had none — it slept
// exactly (attempt+1)*5ms. Two things went wrong with that under the load this
// retry exists for (kernel/eventstore's 1000-writer torture):
//
//   - **Lockstep.** Writers that collide sleep the same duration, wake
//     together, and collide again. The retry storm re-forms every round
//     instead of dispersing.
//   - **The starved writer waited longest.** Backoff grew with a writer's own
//     attempt count, so the one that had lost most often slept up to 200ms
//     while a newly-arrived writer slept 5ms and took the lock ahead of it.
//     That is backwards, and it is how a single writer burns a 30-second
//     budget while every other writer finishes — the exact shape of the CI
//     failure this replaced ("writer 624: gave up after 30s").
//
// Drawing from (0, ceiling] instead of sleeping the ceiling fixes both: colliding writers
// get different delays, and a writer deep into its attempts can still draw a
// short one, so progress does not depend on being lucky early. This is the
// "full jitter" strategy from AWS's backoff-and-jitter work.
func BusyBackoff(attempt int) time.Duration {
	const maxBackoff = 200 * time.Millisecond
	ceiling := time.Duration(attempt+1) * 5 * time.Millisecond
	if ceiling > maxBackoff {
		ceiling = maxBackoff
	}
	// #nosec G404 -- scheduling jitter, not a security decision. Nothing here
	// is a secret and nothing depends on the sequence being unpredictable.
	return time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond
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
		// A cancelled or timed-out request has its transaction rolled back by
		// database/sql's context watcher, which runs in its own goroutine. So
		// Commit reports either the context error or — when that rollback lands
		// first — a bare sql.ErrTxDone, and the second form is indistinguishable
		// from a genuine storage fault by the time it reaches the edge. That is
		// how an ordinary "user navigated away" became an unclassified 500 in
		// the logs (found by the WP-1.10 e2e, once reporting stopped swallowing
		// these).
		//
		// The cause is the cancellation in both cases, so report that and let
		// the edge classify it as the non-event it is.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("tenancy: commit: %w", ctxErr)
		}
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

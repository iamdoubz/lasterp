// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Handler runs one job of a registered kind.
//
// It receives the tenant and the payload and nothing else — in particular no
// actor. A job runs as whatever principal its handler establishes, and every
// handler that writes must establish one (INV-T4): work that resumes minutes
// after the request that queued it cannot inherit that request's session, and
// a queue that carried a caller's authority forward would be a way to act with
// someone's permissions after they logged out.
type Handler func(ctx context.Context, tenant tenancy.ID, payload []byte) error

// ErrUnknownKind is returned when a claimed job names a kind no handler is
// registered for.
var ErrUnknownKind = errors.New("jobs: no handler registered for this kind")

// Registry maps a job kind to its handler. Safe for concurrent use once built.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register adds a handler. Registering a kind twice is a programming error and
// panics: two handlers for one kind means the one that runs depends on
// initialisation order, which is not a thing to discover in production.
func (r *Registry) Register(kind string, h Handler) {
	if kind == "" || h == nil {
		panic("jobs: Register needs a kind and a handler")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[kind]; dup {
		panic("jobs: handler already registered for kind " + kind)
	}
	r.handlers[kind] = h
}

func (r *Registry) lookup(kind string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[kind]
	return h, ok
}

// Runner drains one tenant's queue.
type Runner struct {
	db       *storage.DB
	registry *Registry
	worker   string
	// MaxPerPass bounds one drain so a large backlog cannot starve the other
	// tenants sharing this runner's ticker.
	MaxPerPass int
}

func NewRunner(db *storage.DB, registry *Registry, worker string) *Runner {
	return &Runner{db: db, registry: registry, worker: worker, MaxPerPass: 100}
}

// RunOnce ticks the tenant's schedules, then claims and runs due jobs until the
// queue is empty or MaxPerPass is reached. It reports how many jobs it ran.
//
// A handler's failure is the job's failure, never the pass's: it is recorded
// through Fail (retry with backoff, then a dead letter) and the drain moves on.
// The only errors returned here are storage faults, because those mean the
// queue itself is unreachable and continuing would be pretending otherwise.
func (r *Runner) RunOnce(ctx context.Context, tenant tenancy.ID, now time.Time) (int, error) {
	if _, err := TickSchedules(ctx, r.db, tenant, now); err != nil {
		return 0, err
	}
	ran := 0
	for ran < r.MaxPerPass {
		if err := ctx.Err(); err != nil {
			return ran, err
		}
		job, err := Claim(ctx, r.db, tenant, r.worker, now)
		if err != nil {
			return ran, err
		}
		if job == nil {
			return ran, nil
		}
		if err := r.run(ctx, tenant, job); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

// run executes one claimed job and records the outcome.
func (r *Runner) run(ctx context.Context, tenant tenancy.ID, job *Job) error {
	handler, ok := r.registry.lookup(job.Kind)
	if !ok {
		// Not a crash: a job whose kind is unregistered is what a rolling
		// deploy looks like from the old binary, or an uninstalled plugin's
		// leftover work. It retries, and if the kind never appears it becomes a
		// dead letter naming the kind — which is the message an operator needs.
		return Fail(ctx, r.db, tenant, job.ID, fmt.Errorf("%w: %s", ErrUnknownKind, job.Kind))
	}

	err := runGuarded(ctx, handler, tenant, job.Payload)
	if err != nil {
		return Fail(ctx, r.db, tenant, job.ID, err)
	}
	return Complete(ctx, r.db, tenant, job.ID)
}

// runGuarded turns a panicking handler into a failed job.
//
// A handler is arbitrary code — a plugin invocation, an automation action — and
// a panic in one must not take down the runner and with it every other tenant's
// queue. The recovered value becomes the dead letter's cause, so the panic is
// still visible rather than swallowed.
func runGuarded(ctx context.Context, h Handler, tenant tenancy.ID, payload []byte) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("jobs: handler panicked: %v", rec)
		}
	}()
	return h(ctx, tenant, payload)
}

// Start runs RunOnce for every tenant on a ticker until ctx is cancelled,
// returning a function that waits for it to stop.
//
// It sweeps rather than subscribing, for the reason StartHookRunner gives: a
// sweep has no wake-up to miss, and the tenant list is one query against a
// global table (ADR-005). The ceiling is the same and is stated the same way —
// one pass is O(tenants with work) per tick.
func Start(ctx context.Context, db *storage.DB, registry *Registry, worker string, every time.Duration, onError func(error)) func() {
	if every <= 0 {
		every = time.Second
	}
	if onError == nil {
		onError = func(error) {}
	}
	runner := NewRunner(db, registry, worker)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tenants, err := listTenants(ctx, db)
				if err != nil {
					if ctx.Err() == nil {
						onError(fmt.Errorf("jobs: list tenants: %w", err))
					}
					continue
				}
				for _, tenant := range tenants {
					if _, err := runner.RunOnce(ctx, tenant, time.Now()); err != nil && ctx.Err() == nil {
						// One tenant's storage fault must not stop every other
						// tenant's queue. The job-level failures are already
						// recorded as retries or dead letters.
						onError(fmt.Errorf("jobs: run for tenant %s: %w", tenant, err))
					}
				}
			}
		}
	}()
	return func() { <-done }
}

// listTenants reads the global tenant root table (ADR-005: tenants are not
// themselves tenant-scoped, so this needs no tenant context).
func listTenants(ctx context.Context, db *storage.DB) ([]tenancy.ID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []tenancy.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, tenancy.ID(id))
	}
	return out, rows.Err()
}

// SPDX-License-Identifier: AGPL-3.0-only

// Package jobs is the WP-3.3b durable job queue: the one scheduler in the
// system.
//
// It exists because two callers needed it and neither could own it. WP-3.1b's
// plan review moved the plugin manifest's `schedule:` capability and the
// `enqueue_job` host function here rather than building a runner inside the
// plugin host, because a runner built there would become a second scheduler the
// moment automations needed one — and automations (WP-3.3b's other half) are
// where scheduled triggers were always going to arrive.
//
// It is a table, not a broker: solo mode is one binary (ADR-011), and the whole
// of the concurrency control is an atomic compare-and-set UPDATE that both
// dialects execute identically. See docs/notes/WP-3.3-decisions.md §6.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Status values.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
)

// Queue tuning.
const (
	// MaxAttempts is how many times a job is tried before it is filed as a
	// dead letter. Matches plugins.DeliveryAttempts: a plugin author tuning
	// around one number should not discover a second.
	MaxAttempts = 3
	// LeaseDuration is how long a claim is held before another worker may take
	// the job. A lease rather than a lock: a worker that dies leaves the lease
	// to expire, and nothing has to notice the crash.
	LeaseDuration = 5 * time.Minute
	// retryBase is the first retry delay; each attempt doubles it.
	retryBase = 10 * time.Second
	// retryMax caps the backoff, so a job that keeps failing is still retried
	// on a human timescale rather than receding forever.
	retryMax = 5 * time.Minute
	// claimCandidates is how many due rows one Claim inspects before giving
	// up. Bounds the compare-and-set retry loop when many workers contend.
	claimCandidates = 10
)

// ErrNotFound is returned when a job id does not exist in the tenant.
var ErrNotFound = errors.New("jobs: no such job")

// micros and unmicros convert between time.Time and the integer microseconds
// the schedulable columns store.
//
// The queue is the only place in the tree that compares a timestamp *inside*
// SQL, because the claim has to be a single atomic statement (see Claim).
// Everywhere else a timestamp is read out and compared in Go — which is what
// kernel/identity does with `expires_at`, and it is not a stylistic
// difference: on SQLite a TIMESTAMPTZ round-trips as a Go-formatted string
// whose fractional seconds are variable-length, so `locked_until <= ?` becomes
// a lexicographic comparison that is wrong for exactly the values that decide
// whether a lease has expired. An integer orders identically on both dialects.
// Microseconds rather than nanoseconds because Postgres timestamps are
// microsecond-resolution anyway, so nothing is lost that could round-trip.
func micros(t time.Time) int64 { return t.UTC().UnixMicro() }

func unmicros(us int64) time.Time { return time.UnixMicro(us).UTC() }

// Job is one unit of queued work.
type Job struct {
	ID        string
	Kind      string
	Payload   []byte
	Attempts  int
	RunAt     time.Time
	CreatedAt time.Time
}

// Enqueue adds a job, or returns the id of the existing one when dedupeKey is
// non-empty and already queued for this kind.
//
// Deduplication is the producer's idempotency seam. `enqueue_job` is reachable
// from an async hook, and async delivery is at-least-once by design
// (WP-3.1b) — so without a dedupe key a redelivered hook enqueues its job
// twice, and the ERP does the work twice.
func Enqueue(ctx context.Context, db *storage.DB, tenant tenancy.ID, kind string, payload []byte, runAt time.Time, dedupeKey string) (string, error) {
	if tenant == "" || kind == "" {
		return "", errors.New("jobs: tenant and kind are required")
	}
	if runAt.IsZero() {
		runAt = time.Now().UTC()
	}
	id := idgen.New()
	now := time.Now().UTC()
	key := sql.NullString{String: dedupeKey, Valid: dedupeKey != ""}

	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if dedupeKey != "" {
			// Checked inside the same transaction as the insert, and backed by
			// the unique index rather than trusting the check: two nodes can
			// reach this line at once, and the loser must find the winner's
			// row rather than fail the caller.
			var existing string
			err := tx.QueryRowContext(ctx, db.Rebind(
				`SELECT id FROM jobs WHERE tenant_id = ? AND kind = ? AND dedupe_key = ?`),
				string(tenant), kind, dedupeKey).Scan(&existing)
			if err == nil {
				id = existing
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO jobs (tenant_id, id, kind, payload, dedupe_key, status, run_at_us,
			                  attempts, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`),
			string(tenant), id, kind, string(payload), key, StatusPending, micros(runAt), now, now)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}
	return id, nil
}

// claimable is the predicate for "this job may be picked up now": due, not
// finished, and either never claimed or holding an expired lease. It appears
// twice in the claim — once to choose a candidate and once as the outer guard
// that makes the choice safe — so it is written once here.
const claimable = `run_at_us <= ? AND status <> '` + StatusDone + `'
	AND (status = '` + StatusPending + `' OR locked_until_us IS NULL OR locked_until_us <= ?)`

// Claim takes the next due job for this tenant, or returns nil when there is
// none. worker names the claimant, for operator visibility.
//
// **Exclusivity is a property of the statement, not of a snapshot.** The claim
// is one UPDATE whose subquery picks the candidate and whose outer WHERE
// repeats the same predicate. The repetition is the load-bearing part: a second
// writer blocks on the row, and when it resumes it re-checks that outer
// predicate against the *updated* row — now running, with a live lease — and
// matches nothing. Postgres does this through its concurrent-update re-check,
// SQLite through its statement-level write lock. Neither needs
// `FOR UPDATE SKIP LOCKED`, which SQLite does not have and which would
// therefore have put the one piece of concurrency control that matters onto two
// code paths (decisions §6).
//
// The claimed row is read back by its claim token rather than by id: the token
// is unique to this call, so the read cannot pick up a row some other worker
// claimed in between.
//
// **The double delivery this function's test caught was not in the SQL**, and
// the note is here because the next person to touch a WithTenant callback needs
// it. WithTenant retries the entire callback on SQLITE_BUSY. `claimed` is
// captured from the enclosing scope, so an attempt that claimed a job and then
// failed to commit left it set; the retry found the queue empty, returned nil,
// and Claim handed back a job this worker did not hold and another worker was
// already running. Every WithTenant callback that writes to a captured variable
// has that shape — see DeadLetters for the read-only form of the same bug.
func Claim(ctx context.Context, db *storage.DB, tenant tenancy.ID, worker string, now time.Time) (*Job, error) {
	now = now.UTC()
	lease := now.Add(LeaseDuration)
	var claimed *Job

	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// WithTenant retries this whole callback on SQLITE_BUSY, so every
		// attempt must start from nothing. Without this reset an attempt that
		// claimed a job and then failed to commit leaves `claimed` set; the
		// retry finds the queue empty, returns nil, and Claim hands back a job
		// this worker never actually holds — which another worker is running.
		// That is the double delivery TestConcurrentClaimsNeverDoubleDeliver
		// caught, and it was never in the SQL.
		claimed = nil

		// Losing a race is not "the queue is empty", so a lost race retries.
		// The loop is bounded because a worker that cannot win in this many
		// attempts is better off letting the next tick try than spinning.
		for attempt := 0; attempt < claimCandidates; attempt++ {
			token := idgen.New()
			res, err := tx.ExecContext(ctx, db.Rebind(`
				UPDATE jobs
				SET status = ?, locked_until_us = ?, locked_by = ?, claim_token = ?,
				    attempts = attempts + 1, updated_at = ?
				WHERE tenant_id = ? AND id IN (
					SELECT id FROM jobs
					WHERE tenant_id = ? AND `+claimable+`
					ORDER BY run_at_us, id
					LIMIT 1
				)
				AND `+claimable),
				StatusRunning, micros(lease), worker, token, now,
				string(tenant),
				string(tenant), micros(now), micros(now),
				micros(now), micros(now))
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				// Either nothing is due, or another worker just took what was.
				// One cheap existence check tells the two apart, so an empty
				// queue costs one extra statement and contention costs a retry.
				var due int
				err := tx.QueryRowContext(ctx, db.Rebind(`
					SELECT COUNT(*) FROM (
						SELECT id FROM jobs WHERE tenant_id = ? AND `+claimable+` LIMIT 1
					) AS t`),
					string(tenant), micros(now), micros(now)).Scan(&due)
				if err != nil {
					return err
				}
				if due == 0 {
					return nil
				}
				continue
			}

			var job Job
			var payload string
			var runAtUS int64
			var createdAt storage.Time
			err = tx.QueryRowContext(ctx, db.Rebind(`
				SELECT id, kind, payload, attempts, run_at_us, created_at
				FROM jobs WHERE tenant_id = ? AND claim_token = ?`),
				string(tenant), token).Scan(&job.ID, &job.Kind, &payload, &job.Attempts, &runAtUS, &createdAt)
			if errors.Is(err, sql.ErrNoRows) {
				// Cannot happen: the UPDATE above reported a row and this
				// transaction wrote the token. Treated as "no job" rather than
				// as an error, because a claim that cannot be read back must
				// not become a job that runs with an unknown identity.
				return nil
			}
			if err != nil {
				return err
			}
			job.Payload = []byte(payload)
			job.RunAt = unmicros(runAtUS)
			job.CreatedAt = createdAt.Time
			claimed = &job
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	return claimed, nil
}

// Complete marks a claimed job done.
func Complete(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE jobs SET status = ?, locked_until_us = NULL, locked_by = NULL, updated_at = ?
			WHERE tenant_id = ? AND id = ?`),
			StatusDone, time.Now().UTC(), string(tenant), id)
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: complete %s: %w", id, err)
	}
	return nil
}

// Fail records an attempt that failed. It reschedules with exponential backoff
// while attempts remain, and otherwise files a dead letter and removes the job
// from the queue.
//
// The queue moves on either way. A head that retries forever blocks everything
// behind it, which is the stall INV-S4 counts as a silent drop — and the same
// call WP-3.1b made for hook deliveries.
func Fail(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, cause error) error {
	msg := "unknown"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	now := time.Now().UTC()
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var kind, payload string
		var attempts int
		err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT kind, payload, attempts FROM jobs WHERE tenant_id = ? AND id = ?`),
			string(tenant), id).Scan(&kind, &payload, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		if attempts < MaxAttempts {
			_, err = tx.ExecContext(ctx, db.Rebind(`
				UPDATE jobs
				SET status = ?, run_at_us = ?, locked_until_us = NULL, locked_by = NULL,
				    last_error = ?, updated_at = ?
				WHERE tenant_id = ? AND id = ?`),
				StatusPending, micros(now.Add(backoff(attempts))), msg, now, string(tenant), id)
			return err
		}

		if _, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO job_dead_letters (tenant_id, id, job_id, kind, payload, error, attempts, failed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), idgen.New(), id, kind, payload, msg, attempts, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, db.Rebind(`DELETE FROM jobs WHERE tenant_id = ? AND id = ?`),
			string(tenant), id)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("jobs: fail %s: %w", id, err)
	}
	return nil
}

// backoff is the delay before retrying a job that has failed `attempts` times.
func backoff(attempts int) time.Duration {
	d := retryBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= retryMax {
			return retryMax
		}
	}
	return d
}

// DeadLetter is a job that failed every attempt.
type DeadLetter struct {
	ID       string    `json:"id"`
	JobID    string    `json:"job_id"`
	Kind     string    `json:"kind"`
	Payload  string    `json:"payload"`
	Error    string    `json:"error"`
	Attempts int       `json:"attempts"`
	FailedAt time.Time `json:"failed_at"`
}

// DeadLetters lists a tenant's failed jobs, newest first. kind filters when
// non-empty.
func DeadLetters(ctx context.Context, db *storage.DB, tenant tenancy.ID, kind string) ([]DeadLetter, error) {
	if tenant == "" {
		return nil, errors.New("jobs: tenant is required")
	}
	filter, args := "", []any{string(tenant)}
	if kind != "" {
		filter = " AND kind = ?"
		args = append(args, kind)
	}
	var out []DeadLetter
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Built locally and assigned at the end, never appended to across the
		// callback boundary: WithTenant retries this whole function on
		// SQLITE_BUSY, and a slice captured from the enclosing scope keeps
		// whatever a half-finished attempt already put in it — so the retry
		// returns the first rows twice. Same hazard as the one that made Claim
		// double-deliver, in its read-only form.
		var letters []DeadLetter
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, job_id, kind, payload, error, attempts, failed_at
			FROM job_dead_letters WHERE tenant_id = ?`+filter+`
			ORDER BY failed_at DESC, id`), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d DeadLetter
			var failedAt storage.Time
			if err := rows.Scan(&d.ID, &d.JobID, &d.Kind, &d.Payload, &d.Error, &d.Attempts, &failedAt); err != nil {
				return err
			}
			d.FailedAt = failedAt.Time
			letters = append(letters, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = letters
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: list dead letters: %w", err)
	}
	return out, nil
}

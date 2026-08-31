// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Schedule is a recurring job: a cron expression plus the job it enqueues.
//
// Owner scopes a bulk delete. A plugin's schedules are owned by its principal,
// so uninstalling removes them the same way DeleteRole removes its authority —
// a schedule left behind by an uninstall is work that keeps firing for code
// that is gone.
type Schedule struct {
	ID        string
	Kind      string
	Cron      string
	Payload   []byte
	Owner     string
	NextRunAt time.Time
}

// UpsertSchedule creates or replaces a schedule.
//
// The cron expression is parsed here, so an unparseable one is refused at
// install time rather than discovered by a runner that then has nothing useful
// to do with the error. Replacing an existing schedule recomputes its next
// firing from now: the expression changed, so the old firing is not the answer
// to the new question.
func UpsertSchedule(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Schedule, now time.Time) error {
	if tenant == "" || s.ID == "" || s.Kind == "" || s.Owner == "" {
		return errors.New("jobs: tenant, id, kind and owner are required")
	}
	cron, err := ParseCron(s.Cron)
	if err != nil {
		return err
	}
	next := cron.Next(now)
	if next.IsZero() {
		return fmt.Errorf("jobs: schedule %s: %q can never fire", s.ID, s.Cron)
	}
	ts := now.UTC()
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE job_schedules
			SET kind = ?, cron = ?, payload = ?, owner = ?, next_run_at_us = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ?`),
			s.Kind, s.Cron, string(s.Payload), s.Owner, micros(next), ts,
			string(tenant), s.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO job_schedules (tenant_id, id, kind, cron, payload, owner,
			                           next_run_at_us, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), s.ID, s.Kind, s.Cron, string(s.Payload), s.Owner,
			micros(next), ts, ts)
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: upsert schedule %s: %w", s.ID, err)
	}
	return nil
}

// DeleteSchedulesByOwner removes every schedule belonging to owner.
func DeleteSchedulesByOwner(ctx context.Context, db *storage.DB, tenant tenancy.ID, owner string) error {
	if tenant == "" || owner == "" {
		return errors.New("jobs: tenant and owner are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`DELETE FROM job_schedules WHERE tenant_id = ? AND owner = ?`),
			string(tenant), owner)
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: delete schedules for %s: %w", owner, err)
	}
	return nil
}

// ListSchedules returns a tenant's schedules, by id.
func ListSchedules(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Schedule, error) {
	var out []Schedule
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Built locally, assigned once: WithTenant retries the whole callback
		// on SQLITE_BUSY, and appending to a captured slice returns the first
		// rows twice (see DeadLetters).
		var list []Schedule
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, kind, cron, payload, owner, next_run_at_us
			FROM job_schedules WHERE tenant_id = ? ORDER BY id`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s Schedule
			var payload string
			var nextUS int64
			if err := rows.Scan(&s.ID, &s.Kind, &s.Cron, &payload, &s.Owner, &nextUS); err != nil {
				return err
			}
			s.Payload = []byte(payload)
			s.NextRunAt = unmicros(nextUS)
			list = append(list, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: list schedules: %w", err)
	}
	return out, nil
}

// TickSchedules enqueues a job for every schedule now due and advances each to
// its next firing. It reports how many jobs it enqueued.
//
// **Idempotent by construction.** The enqueued job's dedupe key is the schedule
// id and the firing instant, so two nodes ticking the same tenant at the same
// moment produce one job, not two — and the advance is a compare-and-set on the
// firing we read, so the loser changes nothing. Neither node has to know the
// other exists, which is what keeps this working in every deployment shape from
// solo mode to a fleet.
//
// **A missed window fires once, not once per occurrence.** A deployment down
// for a day owes a daily schedule one run, not twenty-four hourly ones: the
// next firing is computed forward from now, so the backlog collapses. This is
// the same call WP-3.1b made for a plugin's delivery cursor — an install does
// not replay the tenant's history — and for the same reason: a burst of
// catch-up work nobody asked for is worse than a late run.
func TickSchedules(ctx context.Context, db *storage.DB, tenant tenancy.ID, now time.Time) (int, error) {
	now = now.UTC()
	due, err := dueSchedules(ctx, db, tenant, now)
	if err != nil {
		return 0, err
	}

	enqueued := 0
	for _, s := range due {
		cron, err := ParseCron(s.Cron)
		if err != nil {
			// Stored by UpsertSchedule, which parsed it — so this is a row that
			// predates a parser change. Skipped rather than fatal: one bad
			// schedule must not stop every other schedule in the tenant.
			continue
		}
		next := cron.Next(now)
		if next.IsZero() {
			// Can never fire again. Park it far enough out that it stops being
			// scanned; deleting it would lose a row the administrator wrote.
			next = now.AddDate(100, 0, 0)
		}
		// The firing being claimed is the one stored on the row, not `now`:
		// that is what makes the dedupe key stable across two nodes whose
		// clocks differ by a few milliseconds.
		key := fmt.Sprintf("%s@%d", s.ID, micros(s.NextRunAt))
		advanced, err := advanceSchedule(ctx, db, tenant, s.ID, s.NextRunAt, next, now)
		if err != nil {
			return enqueued, err
		}
		if !advanced {
			continue // another node took this firing
		}
		if _, err := Enqueue(ctx, db, tenant, s.Kind, s.Payload, s.NextRunAt, key); err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}

func dueSchedules(ctx context.Context, db *storage.DB, tenant tenancy.ID, now time.Time) ([]Schedule, error) {
	var out []Schedule
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var list []Schedule
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, kind, cron, payload, owner, next_run_at_us
			FROM job_schedules WHERE tenant_id = ? AND next_run_at_us <= ?
			ORDER BY next_run_at_us, id`),
			string(tenant), micros(now))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s Schedule
			var payload string
			var nextUS int64
			if err := rows.Scan(&s.ID, &s.Kind, &s.Cron, &payload, &s.Owner, &nextUS); err != nil {
				return err
			}
			s.Payload = []byte(payload)
			s.NextRunAt = unmicros(nextUS)
			list = append(list, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: list due schedules: %w", err)
	}
	return out, nil
}

// advanceSchedule moves a schedule's next firing, but only if it is still the
// one this pass read. Reports whether it won.
func advanceSchedule(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, from, to, now time.Time) (bool, error) {
	won := false
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		won = false // reset per attempt: WithTenant retries this callback
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE job_schedules SET next_run_at_us = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND next_run_at_us = ?`),
			micros(to), now.UTC(), string(tenant), id, micros(from))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		won = n == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("jobs: advance schedule %s: %w", id, err)
	}
	return won, nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Delivery tuning.
const (
	// DeliveryBatch is how many feed entries one pass reads.
	DeliveryBatch = 100
	// DeliveryAttempts is how many times one entry is offered to a hook before
	// it is filed as a dead letter.
	DeliveryAttempts = 3
	// AsyncTimeout is an async hook's wall-clock budget. Far larger than a sync
	// hook's, because nobody is waiting on the other end of a request.
	AsyncTimeout = 30 * time.Second
)

// Runner delivers `after_*` hooks from the change feed
// (WP-3.1b-decisions.md §5).
//
// It is a feed consumer rather than a queue: changefeed.Read is already
// ordered, resumable and exactly-once-observed under INV-S5, so a per-plugin
// cursor is the whole of the delivery machinery. Solo mode gains no broker.
//
// **Delivery is at-least-once, as docs/05 promises.** Two nodes running this
// can both deliver an entry; the cursor advance is a compare-and-set so the
// window is one entry rather than a page, and `after_*` hooks must be
// idempotent — which is why kv ships in this WP, since a dedupe key needs
// somewhere to live.
type Runner struct {
	host  Host
	stats *Stats
}

func NewRunner(h Host, stats *Stats) *Runner {
	if stats == nil {
		stats = NewStats()
	}
	return &Runner{host: h, stats: stats}
}

// Deliver runs one pass for one tenant and reports how many hook invocations
// it made. It is safe to call concurrently with itself; the cursor CAS is what
// keeps two passes from re-delivering the same page.
func (r *Runner) Deliver(ctx context.Context, tenant tenancy.ID) (int, error) {
	installed, err := LoadAll(ctx, r.host.DB, tenant)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for i := range installed {
		p := &installed[i]
		if !p.Manifest.HasAsyncHooks() {
			continue
		}
		n, err := r.deliverPlugin(ctx, tenant, p)
		delivered += n
		if err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

func (r *Runner) deliverPlugin(ctx context.Context, tenant tenancy.ID, p *Installed) (int, error) {
	cursor, err := readCursor(ctx, r.host.DB, tenant, p.ID)
	if err != nil {
		return 0, err
	}
	changes, err := changefeed.Read(ctx, r.host.DB, tenant, cursor, DeliveryBatch, nil)
	if err != nil {
		return 0, err
	}

	delivered := 0
	for _, c := range changes {
		// A plugin does not react to its own writes (decisions §6). This is why
		// change_feed carries an actor at all: without it the runner cannot
		// tell a plugin's own output from anyone else's, and a hook that writes
		// the object it subscribes to loops forever.
		if c.ActorID == p.Principal() {
			if err := advanceCursor(ctx, r.host.DB, tenant, p.ID, cursor, c.Cursor); err != nil {
				return delivered, err
			}
			cursor = c.Cursor
			continue
		}
		for _, h := range p.Manifest.AsyncHooks(c.Object) {
			if p.BreakerOpen() {
				// Unlike a sync hook, a skipped async delivery is not a
				// decision about someone's write — it is work not done yet, so
				// the cursor stays put and the entry is redelivered when the
				// breaker closes. Nothing is lost, delivery is merely late.
				return delivered, nil
			}
			if err := r.invoke(ctx, tenant, p, h, c); err != nil {
				return delivered, err
			}
			delivered++
		}
		if err := advanceCursor(ctx, r.host.DB, tenant, p.ID, cursor, c.Cursor); err != nil {
			return delivered, err
		}
		cursor = c.Cursor
	}
	return delivered, nil
}

// asyncRequest is what an async hook receives: the change, not the row. The
// feed carries no verb (decisions §2), so a hook that needs to know what
// happened reads current state through object.get.
type asyncRequest struct {
	Object string `json:"object"`
	RefID  string `json:"ref_id"`
	Source string `json:"source"`
	Cursor int64  `json:"cursor"`
}

func (r *Runner) invoke(ctx context.Context, tenant tenancy.ID, p *Installed, h Hook, c changefeed.Change) error {
	body, err := json.Marshal(asyncRequest{
		Object: c.Object, RefID: c.RefID, Source: string(c.Source), Cursor: c.Cursor,
	})
	if err != nil {
		return err
	}
	host := r.host
	host.Limits.Timeout = AsyncTimeout

	var lastErr error
	for attempt := 1; attempt <= DeliveryAttempts; attempt++ {
		start := time.Now()
		_, callErr := Call(ctx, host, tenant, p, h.Fn, body)
		elapsed := time.Since(start)
		if callErr == nil {
			r.stats.Record(p.ID, h.Fn, elapsed, outcomeOK)
			return bumpBreaker(ctx, r.host.DB, tenant, p, 0)
		}
		r.stats.Record(p.ID, h.Fn, elapsed, outcomeFailed)
		lastErr = callErr
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
	}

	// Out of attempts. The entry is filed where a person can see it and the
	// cursor moves on: a queue head that retries forever blocks everything
	// behind it, which is the stall INV-S4 counts as a silent drop.
	if err := deadLetter(ctx, r.host.DB, tenant, p.ID, h.Fn, c, lastErr); err != nil {
		return err
	}
	return bumpBreaker(ctx, r.host.DB, tenant, p, +1)
}

func readCursor(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin string) (int64, error) {
	var cursor int64
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT cursor FROM plugin_deliveries WHERE tenant_id = ? AND plugin_id = ?`),
			string(tenant), plugin).Scan(&cursor)
		if errors.Is(err, sql.ErrNoRows) {
			// A plugin installed today does not replay the tenant's history:
			// it starts at the feed's high-water mark. Anything else means an
			// install fires thousands of hooks for changes nobody expected it
			// to see.
			high, err := changefeed.HighWater(ctx, db, tenant)
			if err != nil {
				return err
			}
			cursor = high
			_, err = tx.ExecContext(ctx, db.Rebind(
				`INSERT INTO plugin_deliveries (tenant_id, plugin_id, cursor, updated_at) VALUES (?, ?, ?, ?)`),
				string(tenant), plugin, cursor, time.Now().UTC())
			return err
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("plugins: read delivery cursor for %s: %w", plugin, err)
	}
	return cursor, nil
}

// advanceCursor moves a plugin's cursor only if it is still where this pass
// last saw it. That compare-and-set is what bounds double delivery to a single
// entry when two nodes run the runner at once.
func advanceCursor(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin string, from, to int64) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE plugin_deliveries SET cursor = ?, updated_at = ?
			 WHERE tenant_id = ? AND plugin_id = ? AND cursor = ?`),
			to, time.Now().UTC(), string(tenant), plugin, from)
		return err
	})
	if err != nil {
		return fmt.Errorf("plugins: advance delivery cursor for %s: %w", plugin, err)
	}
	return nil
}

// DeadLetter is a delivery that failed every attempt.
type DeadLetter struct {
	ID       string    `json:"id"`
	PluginID string    `json:"plugin_id"`
	Fn       string    `json:"fn"`
	Cursor   int64     `json:"cursor"`
	Object   string    `json:"object"`
	RefID    string    `json:"ref_id"`
	Error    string    `json:"error"`
	Attempts int       `json:"attempts"`
	FailedAt time.Time `json:"failed_at"`
}

func deadLetter(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin, fn string, c changefeed.Change, cause error) error {
	msg := "unknown"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO plugin_dead_letters (tenant_id, id, plugin_id, fn, cursor, object, ref_id, error, attempts, failed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), idgen.New(), plugin, fn, c.Cursor, c.Object, c.RefID, msg,
			DeliveryAttempts, time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("plugins: file dead letter for %s: %w", plugin, err)
	}
	return nil
}

// DeadLetters lists a tenant's failed deliveries, newest first.
func DeadLetters(ctx context.Context, db *storage.DB, tenant tenancy.ID, plugin string) ([]DeadLetter, error) {
	if tenant == "" {
		return nil, errors.New("plugins: tenant is required")
	}
	filter, args := "", []any{string(tenant)}
	if plugin != "" {
		filter = " AND plugin_id = ?"
		args = append(args, plugin)
	}
	var out []DeadLetter
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, plugin_id, fn, cursor, object, ref_id, error, attempts, failed_at
			FROM plugin_dead_letters WHERE tenant_id = ?`+filter+`
			ORDER BY failed_at DESC, id`), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d DeadLetter
			var failedAt storage.Time
			if err := rows.Scan(&d.ID, &d.PluginID, &d.Fn, &d.Cursor, &d.Object, &d.RefID,
				&d.Error, &d.Attempts, &failedAt); err != nil {
				return err
			}
			d.FailedAt = failedAt.Time
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: list dead letters: %w", err)
	}
	return out, nil
}

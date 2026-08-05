// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Breaker thresholds (docs/05: "repeated failures trip a circuit breaker and
// notify admins").
const (
	// BreakerThreshold is how many consecutive failures open the breaker.
	BreakerThreshold = 5
	// BreakerCooldown is how long it stays open before one call is let through
	// to see whether the plugin recovered (half-open).
	BreakerCooldown = 5 * time.Minute
)

// principalPrefix is the actor namespace a plugin acts under.
const principalPrefix = "plugin:"

// BreakerOpen reports whether this plugin is currently tripped.
//
// Half-open is expressed by time rather than by a state machine: once the
// cooldown has elapsed the breaker reads as closed, the next call runs, and its
// outcome either resets the counter or re-opens it. There is no third state to
// keep consistent across processes.
func (p Installed) BreakerOpen() bool {
	if p.BreakerOpenedAt == nil {
		return false
	}
	return time.Since(*p.BreakerOpenedAt) < BreakerCooldown
}

// bumpBreaker moves a plugin's failure counter. delta +1 records a failure,
// 0 records a success (which resets the counter and closes the breaker).
//
// Persisted on the plugin row rather than held in memory: a counter that resets
// on restart makes the breaker useless in exactly the case it exists for — a
// plugin bad enough to be taking the process down with it
// (WP-3.1b-decisions.md §4).
func bumpBreaker(ctx context.Context, db *storage.DB, tenant tenancy.ID, p *Installed, delta int) error {
	now := time.Now().UTC()
	failures := 0
	var openedAt *time.Time

	if delta > 0 {
		failures = p.HookFailures + delta
		if failures >= BreakerThreshold {
			// Keep the original trip time across further failures, so the
			// cooldown measures "since it broke", not "since it last failed".
			if p.BreakerOpenedAt != nil && p.BreakerOpen() {
				openedAt = p.BreakerOpenedAt
			} else {
				openedAt = &now
			}
		}
	}

	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var opened any
		if openedAt != nil {
			opened = *openedAt
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE plugins SET hook_failures = ?, breaker_opened_at = ? WHERE tenant_id = ? AND id = ?`),
			failures, opened, string(tenant), p.ID); err != nil {
			return err
		}
		// A breaker opening or closing is a change in what the tenant's data
		// pipeline does, so it is attributable like any other (INV-T4). Only
		// transitions are recorded — a counter ticking from 2 to 3 is noise.
		switch {
		case openedAt != nil && (p.BreakerOpenedAt == nil || !p.BreakerOpen()):
			return recordAudit(ctx, tx, db, tenant, p.ID, "breaker-open", principalPrefix+p.ID,
				map[string]any{"failures": failures})
		case delta == 0 && p.BreakerOpenedAt != nil:
			return recordAudit(ctx, tx, db, tenant, p.ID, "breaker-close", principalPrefix+p.ID, nil)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("plugins: update breaker for %s: %w", p.ID, err)
	}
	p.HookFailures, p.BreakerOpenedAt = failures, openedAt
	return nil
}

// ResetBreaker closes a plugin's breaker by hand, for an administrator who has
// fixed whatever was wrong and does not want to wait out the cooldown.
func ResetBreaker(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, actor string) error {
	if tenant == "" || actor == "" {
		return fmt.Errorf("plugins: tenant and actor are required")
	}
	p, err := Get(ctx, db, tenant, id)
	if err != nil {
		return err
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE plugins SET hook_failures = 0, breaker_opened_at = NULL WHERE tenant_id = ? AND id = ?`),
			string(tenant), p.ID); err != nil {
			return err
		}
		return recordAudit(ctx, tx, db, tenant, p.ID, "breaker-reset", actor, nil)
	})
	if err != nil {
		return fmt.Errorf("plugins: reset breaker for %s: %w", id, err)
	}
	return nil
}

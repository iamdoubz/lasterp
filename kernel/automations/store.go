// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrNotFound is returned when a tenant has no automation by that id.
var ErrNotFound = errors.New("automations: no such automation")

// Stored is an automation as persisted.
type Stored struct {
	Definition *Definition
	YAML       string
	Enabled    bool
	CreatedAt  time.Time
	CreatedBy  string
}

// Save creates or replaces an automation.
//
// The cursor is written here, at creation, not lazily on the first delivery
// pass. A cursor created on first pass silently skips everything between the
// save and that pass — the window WP-3.1b found the hard way — and an
// automation starts at the feed's high-water mark rather than replaying the
// tenant's history, because an automation created today reacting to last year's
// invoices is nobody's intent.
func Save(ctx context.Context, db *storage.DB, tenant tenancy.ID, yamlDef []byte, approver authz.Actor) (*Definition, error) {
	if tenant == "" || approver.TenantID == "" || approver.UserID == "" {
		return nil, errors.New("automations: tenant and an approving actor are required")
	}
	d, err := Parse(yamlDef)
	if err != nil {
		return nil, err
	}
	actor := string(approver.UserID)

	// INV-T3, at the only moment this automation's authority is created: it
	// acts as its own principal, so it needs its own grants — and it may not
	// hold one the person creating it does not. An automation is far easier to
	// create than a plugin is to install, which makes this bound *more*
	// important here, not less: without it, "create an automation" is a
	// privilege-escalation primitive for anyone who can reach the route.
	//
	// Refused rather than narrowed, for the reason plugins.Install refuses:
	// silently granting less produces an automation that "saved fine" and then
	// fails every pass for reasons nobody can see.
	if err := grantAuthority(ctx, db, tenant, d, approver); err != nil {
		return nil, err
	}

	high, err := changefeed.HighWater(ctx, db, tenant)
	if err != nil {
		return nil, fmt.Errorf("automations: read feed position: %w", err)
	}
	now := time.Now().UTC()

	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE automations
			SET name = ?, definition = ?, trigger_kind = ?, trigger_object = ?,
			    enabled = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ?`),
			d.Name, string(yamlDef), d.TriggerKind(), d.Trigger.Object, d.IsEnabled(), now,
			string(tenant), d.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, db.Rebind(`
				INSERT INTO automations (tenant_id, id, name, definition, trigger_kind,
				                         trigger_object, enabled, created_at, updated_at, created_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				string(tenant), d.ID, d.Name, string(yamlDef), d.TriggerKind(),
				d.Trigger.Object, d.IsEnabled(), now, now, actor); err != nil {
				return err
			}
		}
		// The cursor is created once and never reset by a later edit: an
		// automation whose condition is corrected should not re-process
		// everything it already declined.
		_, err = tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO automation_cursors (tenant_id, automation_id, cursor, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (tenant_id, automation_id) DO NOTHING`),
			string(tenant), d.ID, high, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("automations: save %s: %w", d.ID, err)
	}
	return d, nil
}

// Get loads one automation.
func Get(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (*Stored, error) {
	var out *Stored
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		out = nil // reset per attempt: WithTenant retries this callback
		var yamlDef, createdBy string
		var enabled bool
		var createdAt storage.Time
		err := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT definition, enabled, created_at, created_by
			FROM automations WHERE tenant_id = ? AND id = ?`),
			string(tenant), id).Scan(&yamlDef, &enabled, &createdAt, &createdBy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		d, err := Parse([]byte(yamlDef))
		if err != nil {
			return err
		}
		out = &Stored{Definition: d, YAML: yamlDef, Enabled: enabled,
			CreatedAt: createdAt.Time, CreatedBy: createdBy}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("automations: get %s: %w", id, err)
	}
	return out, nil
}

// List returns a tenant's automations, by id. Only enabled ones when
// enabledOnly.
func List(ctx context.Context, db *storage.DB, tenant tenancy.ID, enabledOnly bool) ([]Stored, error) {
	var out []Stored
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Built locally and assigned once: WithTenant retries the whole callback
		// on SQLITE_BUSY, and a slice captured from the enclosing scope keeps
		// what a half-finished attempt put in it.
		var list []Stored
		filter := ""
		if enabledOnly {
			filter = " AND enabled = TRUE"
		}
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT definition, enabled, created_at, created_by
			FROM automations WHERE tenant_id = ?`+filter+` ORDER BY id`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var yamlDef, createdBy string
			var enabled bool
			var createdAt storage.Time
			if err := rows.Scan(&yamlDef, &enabled, &createdAt, &createdBy); err != nil {
				return err
			}
			d, err := Parse([]byte(yamlDef))
			if err != nil {
				// A stored definition this build cannot parse is skipped rather
				// than fatal: one automation written against a newer schema
				// must not stop every other automation in the tenant.
				continue
			}
			list = append(list, Stored{Definition: d, YAML: yamlDef, Enabled: enabled,
				CreatedAt: createdAt.Time, CreatedBy: createdBy})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("automations: list: %w", err)
	}
	return out, nil
}

// Delete removes an automation, the authority that came with it, and its
// cursor.
//
// The role goes first and goes always: a role left behind is authority a later
// automation created under the same id would silently inherit, which is the
// same reason plugins.Uninstall deletes its own.
func Delete(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	if err := revokeAuthority(ctx, db, tenant, id); err != nil {
		return err
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM automations WHERE tenant_id = ? AND id = ?`,
			// The cursor goes with it, for the reason a plugin's does: one left
			// behind would make a re-created automation under the same id
			// resume from a position it never chose.
			`DELETE FROM automation_cursors WHERE tenant_id = ? AND automation_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, db.Rebind(stmt), string(tenant), id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("automations: delete %s: %w", id, err)
	}
	return nil
}

// readCursor returns an automation's feed position.
func readCursor(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (int64, error) {
	var cursor int64
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		cursor = 0 // reset per attempt
		err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT cursor FROM automation_cursors WHERE tenant_id = ? AND automation_id = ?`),
			string(tenant), id).Scan(&cursor)
		if errors.Is(err, sql.ErrNoRows) {
			// Save writes the cursor, so this is an automation whose row
			// predates that. Start at the high-water mark rather than replaying.
			high, err := changefeed.HighWater(ctx, db, tenant)
			if err != nil {
				return err
			}
			cursor = high
			_, err = tx.ExecContext(ctx, db.Rebind(
				`INSERT INTO automation_cursors (tenant_id, automation_id, cursor, updated_at)
				 VALUES (?, ?, ?, ?)`),
				string(tenant), id, cursor, time.Now().UTC())
			return err
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("automations: read cursor for %s: %w", id, err)
	}
	return cursor, nil
}

// advanceCursor moves the cursor only if it is still where this pass saw it.
// The compare-and-set is what bounds double processing to a single entry when
// two nodes run the runner at once — the same guarantee, and the same
// mechanism, as the plugin delivery cursor.
func advanceCursor(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, from, to int64) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`UPDATE automation_cursors SET cursor = ?, updated_at = ?
			 WHERE tenant_id = ? AND automation_id = ? AND cursor = ?`),
			to, time.Now().UTC(), string(tenant), id, from)
		return err
	})
	if err != nil {
		return fmt.Errorf("automations: advance cursor for %s: %w", id, err)
	}
	return nil
}

// Run outcomes.
const (
	OutcomeMatched = "matched"
	OutcomeSkipped = "skipped"
	OutcomeFailed  = "failed"
)

// Run is one recorded firing.
type Run struct {
	ID           string    `json:"id"`
	AutomationID string    `json:"automation_id"`
	TriggerRef   string    `json:"trigger_ref"`
	Outcome      string    `json:"outcome"`
	Detail       string    `json:"detail"`
	RanAt        time.Time `json:"ran_at"`
}

func recordRun(ctx context.Context, db *storage.DB, tenant tenancy.ID, automationID, ref, outcome, detail string) error {
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO automation_runs (tenant_id, id, automation_id, trigger_ref, outcome, detail, ran_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), idgen.New(), automationID, ref, outcome, detail, time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("automations: record run: %w", err)
	}
	return nil
}

// Runs lists an automation's recent firings, newest first.
func Runs(ctx context.Context, db *storage.DB, tenant tenancy.ID, automationID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Run
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var list []Run
		filter, args := "", []any{string(tenant)}
		if automationID != "" {
			filter = " AND automation_id = ?"
			args = append(args, automationID)
		}
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, automation_id, trigger_ref, outcome, detail, ran_at
			FROM automation_runs WHERE tenant_id = ?`+filter+`
			ORDER BY ran_at DESC, id LIMIT ?`), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r Run
			var ranAt storage.Time
			if err := rows.Scan(&r.ID, &r.AutomationID, &r.TriggerRef, &r.Outcome, &r.Detail, &ranAt); err != nil {
				return err
			}
			r.RanAt = ranAt.Time
			list = append(list, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("automations: list runs: %w", err)
	}
	return out, nil
}

// ErrCapabilityNotHeld is returned when a definition needs authority the person
// creating it does not themselves hold.
var ErrCapabilityNotHeld = errors.New("automations: the creator does not hold a permission this automation needs")

// grantAuthority creates (or replaces) the automation's role and grants it
// exactly the permissions its definition needs, bounded by the approver's own.
//
// The role is replaced rather than amended on every save, so an edit that drops
// an action drops the grant that went with it. Authority follows the
// definition; nothing accumulates.
func grantAuthority(ctx context.Context, db *storage.DB, tenant tenancy.ID, d *Definition, approver authz.Actor) error {
	perms := d.Permissions()
	var missing []string
	for _, p := range perms {
		ok, err := authz.Can(ctx, db, approver, p[0], p[1])
		if err != nil {
			return fmt.Errorf("automations: check creator grant %s:%s: %w", p[0], p[1], err)
		}
		if !ok {
			missing = append(missing, p[0]+":"+p[1])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrCapabilityNotHeld, strings.Join(missing, ", "))
	}

	if err := revokeAuthority(ctx, db, tenant, d.ID); err != nil {
		return err
	}
	role, err := authz.CreateRole(ctx, db, tenant, roleName(d.ID), false)
	if err != nil {
		return fmt.Errorf("automations: create role: %w", err)
	}
	for _, p := range perms {
		if err := authz.GrantPermission(ctx, db, tenant, role, p[0], p[1], ""); err != nil {
			return fmt.Errorf("automations: grant %s:%s: %w", p[0], p[1], err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, identity.UserID(PrincipalFor(d.ID)), role); err != nil {
		return fmt.Errorf("automations: assign role: %w", err)
	}
	return nil
}

// revokeAuthority removes the automation's role, its grants and its assignment.
// A role left behind is authority a later automation under the same id would
// silently inherit — the same reason plugins.Uninstall deletes its own.
func revokeAuthority(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	role, err := authz.RoleByName(ctx, db, tenant, roleName(id))
	if err != nil {
		if errors.Is(err, authz.ErrRoleNotFound) {
			return nil
		}
		return fmt.Errorf("automations: look up role: %w", err)
	}
	if err := authz.DeleteRole(ctx, db, tenant, role); err != nil {
		return fmt.Errorf("automations: delete role: %w", err)
	}
	return nil
}

func roleName(id string) string { return PrincipalFor(id) }

// SPDX-License-Identifier: AGPL-3.0-only

package capability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// authzObject/authzAction are the permission a principal needs to change a
// tenant's enabled modules. Enable/Disable/ApplyProfile all go through
// authz.Authorize (INV-T2) and record an audit row (INV-T4): a capability
// change is an attributable mutation like any other.
const (
	authzObject = "capability"
	authzAction = "manage"
)

// ErrModuleInUse is returned by Disable when other enabled modules depend on
// the target; disabling would break them (their names are listed).
type ErrModuleInUse struct{ DependentModules []string }

func (e ErrModuleInUse) Error() string {
	return fmt.Sprintf("capability: module is required by: %s", strings.Join(e.DependentModules, ", "))
}

// ErrProfilePending is returned by ApplyProfile for a profile that is not yet
// bootable (a capability it needs has no provider — e.g. documents.ocr).
var ErrProfilePending = errors.New("capability: profile is not yet bootable")

// EnabledModules returns the tenant's currently-enabled module names, sorted.
// A tenant with no rows has nothing enabled until a profile is applied.
func EnabledModules(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]string, error) {
	var mods []string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		mods, err = readEnabled(ctx, tx, db, tenant)
		return err
	})
	if err != nil {
		return nil, err
	}
	return mods, nil
}

// IsModuleEnabled reports whether module is enabled for tenant.
func IsModuleEnabled(ctx context.Context, db *storage.DB, tenant tenancy.ID, module string) (bool, error) {
	mods, err := EnabledModules(ctx, db, tenant)
	if err != nil {
		return false, err
	}
	return set(mods)[module], nil
}

// IsCapabilityEnabled reports whether cap is available to tenant: kernel
// capabilities are always on; otherwise the providing module must be enabled.
func IsCapabilityEnabled(ctx context.Context, db *storage.DB, reg *Registry, tenant tenancy.ID, cap string) (bool, error) {
	prov, ok := reg.providers[cap]
	if !ok {
		return false, nil
	}
	if m := reg.modules[prov]; m != nil && m.Kernel {
		return true, nil
	}
	return IsModuleEnabled(ctx, db, tenant, prov)
}

// Enable turns module on together with its dependency closure, returning the
// preview of what was newly enabled (ADR-018 §3). Idempotent: re-enabling an
// already-on module is a no-op with an empty Added set.
func Enable(ctx context.Context, db *storage.DB, reg *Registry, tenant tenancy.ID, module string) (EnableResult, error) {
	actor, err := authz.Authorize(ctx, db, authzObject, authzAction)
	if err != nil {
		return EnableResult{}, err
	}
	var result EnableResult
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readEnabled(ctx, tx, db, tenant)
		if err != nil {
			return err
		}
		result, err = reg.EnableClosure(current, module)
		if err != nil {
			return err
		}
		for _, m := range result.Added {
			if err := upsertModule(ctx, tx, db, tenant, m, true, string(actor.UserID)); err != nil {
				return err
			}
		}
		if len(result.Added) == 0 {
			return nil
		}
		return recordAudit(ctx, tx, db, tenant, module, "enable", result.Added, string(actor.UserID))
	})
	if err != nil {
		return EnableResult{}, err
	}
	return result, nil
}

// Disable turns module off, retaining its data (ADR-018 §5). It refuses with
// ErrModuleInUse if other enabled modules depend on it, and
// ErrKernelNotDisableable for the kernel.
func Disable(ctx context.Context, db *storage.DB, reg *Registry, tenant tenancy.ID, module string) error {
	actor, err := authz.Authorize(ctx, db, authzObject, authzAction)
	if err != nil {
		return err
	}
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readEnabled(ctx, tx, db, tenant)
		if err != nil {
			return err
		}
		if !set(current)[module] {
			return nil // already off (or never on): nothing to do
		}
		broken, err := reg.DisableImpact(current, module)
		if err != nil {
			return err
		}
		if len(broken) > 0 {
			return ErrModuleInUse{DependentModules: broken}
		}
		if err := upsertModule(ctx, tx, db, tenant, module, false, string(actor.UserID)); err != nil {
			return err
		}
		return recordAudit(ctx, tx, db, tenant, module, "disable", []string{module}, string(actor.UserID))
	})
}

// ApplyProfile enables a whole profile's closure at once (ADR-018 §6). The
// profile must be bootable (ErrProfilePending otherwise).
func ApplyProfile(ctx context.Context, db *storage.DB, reg *Registry, tenant tenancy.ID, p Profile) ([]string, error) {
	if !p.Bootable() {
		return nil, fmt.Errorf("%w: %s needs %q", ErrProfilePending, p.Name, p.Pending)
	}
	actor, err := authz.Authorize(ctx, db, authzObject, authzAction)
	if err != nil {
		return nil, err
	}
	closure, err := reg.ProfileClosure(p)
	if err != nil {
		return nil, err
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		for _, m := range closure {
			if err := upsertModule(ctx, tx, db, tenant, m, true, string(actor.UserID)); err != nil {
				return err
			}
		}
		return recordAudit(ctx, tx, db, tenant, p.Name, "apply_profile", closure, string(actor.UserID))
	})
	if err != nil {
		return nil, err
	}
	return closure, nil
}

func readEnabled(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID) ([]string, error) {
	rows, err := tx.QueryContext(ctx, db.Rebind(`SELECT module FROM module_state WHERE tenant_id = ? AND enabled = ?`),
		string(tenant), true)
	if err != nil {
		return nil, fmt.Errorf("capability: read enabled: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var mods []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		mods = append(mods, m)
	}
	return mods, rows.Err()
}

// upsertModule writes the module's enable-state. INSERT ... ON CONFLICT DO
// UPDATE is supported identically by Postgres and SQLite (modernc), so one
// dialect-neutral statement covers both. Disable flips enabled to false and
// keeps the row — data is never deleted.
func upsertModule(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, module string, enabled bool, actorID string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO module_state (tenant_id, module, enabled, mode, updated_at, updated_by)
		VALUES (?, ?, ?, '', ?, ?)
		ON CONFLICT (tenant_id, module)
		DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at, updated_by = excluded.updated_by`),
		string(tenant), module, enabled, time.Now().UTC(), actorID)
	if err != nil {
		return fmt.Errorf("capability: upsert module %q: %w", module, err)
	}
	return nil
}

// recordAudit writes one attributable audit_log row for a capability change
// (INV-T4), same shape as kernel/metadata's CRUD audit.
func recordAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, target, action string, modules []string, actorID string) error {
	changes, err := json.Marshal(map[string]any{"modules": modules})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), "module_state", target, action, string(changes), actorID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("capability: audit %s: %w", action, err)
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// RoleID identifies a tenant-scoped role.
type RoleID string

// ErrCorePermissionFloor is returned when revoking a permission from a
// core role — INV-T3: permission floors cannot be lowered by overlays or
// tenant admins.
var ErrCorePermissionFloor = errors.New("authz: core role permissions cannot be revoked")

// ErrPermissionDenied is returned by Authorize when the actor lacks the
// requested permission.
var ErrPermissionDenied = errors.New("authz: permission denied")

// CreateRole creates a tenant-scoped role. isCore marks a seeded role
// whose permission floor Authorize/RevokePermission will not let the
// tenant-facing API lower.
func CreateRole(ctx context.Context, db *storage.DB, tenant tenancy.ID, name string, isCore bool) (RoleID, error) {
	if tenant == "" || name == "" {
		return "", errors.New("authz: tenant and name are required")
	}
	id := RoleID(idgen.New())
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`INSERT INTO roles (id, tenant_id, name, is_core) VALUES (?, ?, ?, ?)`),
			string(id), string(tenant), name, isCore)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("authz: create role: %w", err)
	}
	return id, nil
}

// GrantPermission grants role the (object, action) permission.
//
// condition is an optional CEL expression over `record` and `actor` (docs/08
// §AuthZ). It is compiled here and refused if it does not yield a boolean
// (ErrConditionInvalid), or if it is attached to an action whose gate has no
// record to judge (ErrConditionUnsupportedAction) — a rule the gate would never
// consult is a permission that reads as narrower than it is. A condition can
// only ever narrow the grant it sits on; see GrantSet.Allow (INV-T3).
func GrantPermission(ctx context.Context, db *storage.DB, tenant tenancy.ID, role RoleID, object, action, condition string) error {
	if tenant == "" || role == "" || object == "" || action == "" {
		return errors.New("authz: tenant, role, object and action are required")
	}
	if err := validateCondition(action, condition); err != nil {
		return err
	}
	stored := sql.NullString{String: condition, Valid: condition != ""}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO role_permissions (id, tenant_id, role_id, object, action, condition)
			VALUES (?, ?, ?, ?, ?, ?)`),
			idgen.New(), string(tenant), string(role), object, action, stored)
		return err
	})
	if err != nil {
		return fmt.Errorf("authz: grant permission: %w", err)
	}
	return nil
}

// RevokePermission removes a grant, unless role is a core role
// (ErrCorePermissionFloor).
func RevokePermission(ctx context.Context, db *storage.DB, tenant tenancy.ID, role RoleID, object, action string) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var isCore bool
		row := tx.QueryRowContext(ctx, db.Rebind(`SELECT is_core FROM roles WHERE tenant_id = ? AND id = ?`), string(tenant), string(role))
		if err := row.Scan(&isCore); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("authz: lookup role: %w", err)
		}
		if isCore {
			return ErrCorePermissionFloor
		}
		_, err := tx.ExecContext(ctx, db.Rebind(`
			DELETE FROM role_permissions WHERE tenant_id = ? AND role_id = ? AND object = ? AND action = ?`),
			string(tenant), string(role), object, action)
		if err != nil {
			return fmt.Errorf("authz: revoke permission: %w", err)
		}
		return nil
	})
	return err
}

// AssignRole grants role to user.
func AssignRole(ctx context.Context, db *storage.DB, tenant tenancy.ID, user identity.UserID, role RoleID) error {
	if tenant == "" || user == "" || role == "" {
		return errors.New("authz: tenant, user and role are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`INSERT INTO user_roles (tenant_id, user_id, role_id) VALUES (?, ?, ?)`),
			string(tenant), string(user), string(role))
		return err
	})
	if err != nil {
		return fmt.Errorf("authz: assign role: %w", err)
	}
	return nil
}

// Can reports whether actor holds an **unconditional** grant for
// (object, action) through any assigned role.
//
// A conditional grant deliberately does not satisfy it. Can has no record, so a
// condition over `record.*` cannot be evaluated here, and treating the grant as
// sufficient anyway is exactly the widening INV-T3 forbids — it is the shape
// the bug would actually take. Record-bearing callers use AuthorizeGrants or
// AuthorizeRecord (condition.go); everyone else keeps WP-0.3's behaviour
// unchanged, since before WP-3.3a no condition could be stored at all.
func Can(ctx context.Context, db *storage.DB, actor Actor, object, action string) (bool, error) {
	if !actor.valid() {
		return false, ErrNoActor
	}
	var n int
	err := tenancy.WithTenant(ctx, db, actor.TenantID, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT COUNT(*) FROM role_permissions rp
			JOIN user_roles ur ON ur.role_id = rp.role_id AND ur.tenant_id = rp.tenant_id
			WHERE rp.tenant_id = ? AND ur.user_id = ? AND rp.object = ? AND rp.action = ?
			  AND (rp.condition IS NULL OR rp.condition = '')`),
			string(actor.TenantID), string(actor.UserID), object, action)
		return row.Scan(&n)
	})
	if err != nil {
		return false, fmt.Errorf("authz: check permission: %w", err)
	}
	return n > 0, nil
}

// GrantedObjects lists, sorted, every object actor holds the given action on
// through any assigned role.
//
// It is Can inverted: one query instead of one per candidate object, which is
// what a caller building a *set* needs. WP-2.4's sync scope is that caller —
// "which objects may this principal read" is asked on every feed page, and
// asking it object-by-object is a query per object per request.
//
// It is not an authorization decision and nothing may treat it as one: Can and
// Authorize remain the only gates (INV-T2). This answers a question about
// grants; a gate answers a question about one access.
//
// Like Can, it counts **unconditional** grants only. A conditionally-granted
// object listed here would hand a replica the rows the condition denies —
// INV-T1/T2 through the sync door — and the scope answer is object-level, so it
// has nowhere to put a row predicate.
func GrantedObjects(ctx context.Context, db *storage.DB, actor Actor, action string) ([]string, error) {
	if !actor.valid() {
		return nil, ErrNoActor
	}
	if action == "" {
		return nil, errors.New("authz: action is required")
	}
	var objects []string
	err := tenancy.WithTenant(ctx, db, actor.TenantID, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT DISTINCT rp.object FROM role_permissions rp
			JOIN user_roles ur ON ur.role_id = rp.role_id AND ur.tenant_id = rp.tenant_id
			WHERE rp.tenant_id = ? AND ur.user_id = ? AND rp.action = ?
			  AND (rp.condition IS NULL OR rp.condition = '')
			ORDER BY rp.object`),
			string(actor.TenantID), string(actor.UserID), action)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []string
		for rows.Next() {
			var object string
			if err := rows.Scan(&object); err != nil {
				return err
			}
			list = append(list, object)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		objects = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("authz: list granted objects: %w", err)
	}
	return objects, nil
}

// Authorize is the single choke point write paths call before mutating
// anything: it requires both an attributable actor (INV-T4, via
// ActorFromContext) and an explicit permission grant (INV-T2, via Can) —
// never one without the other.
func Authorize(ctx context.Context, db *storage.DB, object, action string) (Actor, error) {
	actor, err := ActorFromContext(ctx)
	if err != nil {
		return Actor{}, err
	}
	ok, err := Can(ctx, db, actor, object, action)
	if err != nil {
		return Actor{}, err
	}
	if !ok {
		return Actor{}, ErrPermissionDenied
	}
	return actor, nil
}

// RolesFor lists the names of the roles assigned to actor, sorted.
//
// It answers "who is this person here", which is what lets the product open the
// dashboard belonging to someone's role (docs/21 §4). It is deliberately not an
// authorization primitive: role *names* are tenant-chosen strings, so nothing
// may grant access on the strength of one. Can/Authorize remain the only gates
// (INV-T2).
func RolesFor(ctx context.Context, db *storage.DB, actor Actor) ([]string, error) {
	if !actor.valid() {
		return nil, ErrNoActor
	}
	var names []string
	err := tenancy.WithTenant(ctx, db, actor.TenantID, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT r.name FROM roles r
			JOIN user_roles ur ON ur.role_id = r.id AND ur.tenant_id = r.tenant_id
			WHERE r.tenant_id = ? AND ur.user_id = ?
			ORDER BY r.name`),
			string(actor.TenantID), string(actor.UserID))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			list = append(list, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		names = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("authz: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role, its grants and every assignment of it, in one
// transaction. Core roles are refused (ErrCorePermissionFloor) for the same
// reason RevokePermission refuses them: a permission floor that can be deleted
// wholesale is not a floor.
//
// Added by WP-3.1a, whose uninstall needs it: a plugin's authority lives in a
// role, and a role left behind by an uninstall is authority a reinstall of a
// *different* plugin under the same id would silently inherit.
func DeleteRole(ctx context.Context, db *storage.DB, tenant tenancy.ID, role RoleID) error {
	if tenant == "" || role == "" {
		return errors.New("authz: tenant and role are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var isCore bool
		row := tx.QueryRowContext(ctx, db.Rebind(`SELECT is_core FROM roles WHERE tenant_id = ? AND id = ?`),
			string(tenant), string(role))
		if err := row.Scan(&isCore); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // already gone; deleting twice is not an error
			}
			return fmt.Errorf("authz: lookup role: %w", err)
		}
		if isCore {
			return ErrCorePermissionFloor
		}
		for _, stmt := range []string{
			`DELETE FROM role_permissions WHERE tenant_id = ? AND role_id = ?`,
			`DELETE FROM user_roles WHERE tenant_id = ? AND role_id = ?`,
			`DELETE FROM roles WHERE tenant_id = ? AND id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, db.Rebind(stmt), string(tenant), string(role)); err != nil {
				return fmt.Errorf("authz: delete role: %w", err)
			}
		}
		return nil
	})
	return err
}

// ErrRoleNotFound is returned by RoleByName when the tenant has no such role.
var ErrRoleNotFound = errors.New("authz: no such role")

// RoleByName resolves a role id from its tenant-unique name.
//
// It exists for the principals whose authority is derived from a definition
// rather than stored beside it: kernel/automations names its role after the
// automation, so replacing or deleting one needs the id without carrying a
// column for it. Roles are unique per (tenant_id, name) — idx_roles_tenant_name
// — so the answer is exact.
//
// It is not an authorization primitive and nothing may treat it as one: role
// names are tenant-chosen strings. Can and Authorize remain the only gates
// (INV-T2).
func RoleByName(ctx context.Context, db *storage.DB, tenant tenancy.ID, name string) (RoleID, error) {
	if tenant == "" || name == "" {
		return "", errors.New("authz: tenant and name are required")
	}
	var id string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		id = "" // reset per attempt: WithTenant retries this callback
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT id FROM roles WHERE tenant_id = ? AND name = ?`),
			string(tenant), name).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRoleNotFound
	}
	if err != nil {
		return "", fmt.Errorf("authz: look up role %q: %w", name, err)
	}
	return RoleID(id), nil
}

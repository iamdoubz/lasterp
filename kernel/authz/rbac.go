// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// RoleID identifies a tenant-scoped role.
type RoleID string

// ErrConditionNotSupported is returned by GrantPermission for any
// non-empty condition: role_permissions.condition is stored for
// forward-compatibility with the CEL-based conditions described in
// docs/08-SECURITY-MULTITENANCY.md, but evaluation isn't implemented yet
// (docs/notes/WP-0.3-decisions.md) — rejecting outright avoids silently
// granting an unconditional permission the caller meant to restrict.
var ErrConditionNotSupported = errors.New("authz: conditional grants are not yet supported")

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
	id := RoleID(uuid.NewString())
	_, err := db.ExecContext(ctx, db.Rebind(`INSERT INTO roles (id, tenant_id, name, is_core) VALUES (?, ?, ?, ?)`),
		string(id), string(tenant), name, isCore)
	if err != nil {
		return "", fmt.Errorf("authz: create role: %w", err)
	}
	return id, nil
}

// GrantPermission grants role the (object, action) permission,
// unconditionally only — see ErrConditionNotSupported.
func GrantPermission(ctx context.Context, db *storage.DB, tenant tenancy.ID, role RoleID, object, action, condition string) error {
	if condition != "" {
		return ErrConditionNotSupported
	}
	if tenant == "" || role == "" || object == "" || action == "" {
		return errors.New("authz: tenant, role, object and action are required")
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		INSERT INTO role_permissions (id, tenant_id, role_id, object, action, condition)
		VALUES (?, ?, ?, ?, ?, NULL)`),
		uuid.NewString(), string(tenant), string(role), object, action)
	if err != nil {
		return fmt.Errorf("authz: grant permission: %w", err)
	}
	return nil
}

// RevokePermission removes a grant, unless role is a core role
// (ErrCorePermissionFloor).
func RevokePermission(ctx context.Context, db *storage.DB, tenant tenancy.ID, role RoleID, object, action string) error {
	var isCore bool
	row := db.QueryRowContext(ctx, db.Rebind(`SELECT is_core FROM roles WHERE tenant_id = ? AND id = ?`), string(tenant), string(role))
	if err := row.Scan(&isCore); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("authz: lookup role: %w", err)
	}
	if isCore {
		return ErrCorePermissionFloor
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		DELETE FROM role_permissions WHERE tenant_id = ? AND role_id = ? AND object = ? AND action = ?`),
		string(tenant), string(role), object, action)
	if err != nil {
		return fmt.Errorf("authz: revoke permission: %w", err)
	}
	return nil
}

// AssignRole grants role to user.
func AssignRole(ctx context.Context, db *storage.DB, tenant tenancy.ID, user identity.UserID, role RoleID) error {
	if tenant == "" || user == "" || role == "" {
		return errors.New("authz: tenant, user and role are required")
	}
	_, err := db.ExecContext(ctx, db.Rebind(`INSERT INTO user_roles (tenant_id, user_id, role_id) VALUES (?, ?, ?)`),
		string(tenant), string(user), string(role))
	if err != nil {
		return fmt.Errorf("authz: assign role: %w", err)
	}
	return nil
}

// Can reports whether actor holds a grant for (object, action) through
// any assigned role.
func Can(ctx context.Context, db *storage.DB, actor Actor, object, action string) (bool, error) {
	if !actor.valid() {
		return false, ErrNoActor
	}
	var n int
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT COUNT(*) FROM role_permissions rp
		JOIN user_roles ur ON ur.role_id = rp.role_id AND ur.tenant_id = rp.tenant_id
		WHERE rp.tenant_id = ? AND ur.user_id = ? AND rp.object = ? AND rp.action = ?`),
		string(actor.TenantID), string(actor.UserID), object, action)
	if err := row.Scan(&n); err != nil {
		return false, fmt.Errorf("authz: check permission: %w", err)
	}
	return n > 0, nil
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

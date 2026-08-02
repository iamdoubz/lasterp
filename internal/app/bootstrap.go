// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// BootstrapInput describes the first tenant and administrator to create.
type BootstrapInput struct {
	Tenant   string // tenant id, also what the login form asks for
	Name     string // human-readable tenant name
	Email    string
	Password string
	// Modules to enable for the new tenant. Empty means the built-in set.
	Modules []string
}

// defaultModules is what a new tenant gets so the product is usable on first
// login rather than a shell with every screen switched off (a fresh tenant has
// zero capabilities enabled — WP-1.4b-decisions.md §5).
var defaultModules = []string{"contacts", "ledger", "tax-engine", "invoicing"}

// adminGrants is the first administrator's permission set. It is written out
// rather than granted by wildcard on purpose: authz has no wildcard, and a
// bootstrap that silently grants whatever objects happen to exist would widen
// itself every time a module is added. Adding a module means deciding, here,
// whether the first admin should be able to use it.
var adminGrants = map[string][]string{
	"Account":      {"create", "read", "update", "delete"},
	"Contact":      {"create", "read", "update", "delete"},
	"Period":       {"create", "read", "update"},
	"Invoice":      {"create", "read", "update", "post"},
	"Receipt":      {"create", "read", "update", "post"},
	"JournalEntry": {"post", "read", "reverse"},
	"capability":   {"manage"},
	"TaxRate":      {"manage"},
	"FxRate":       {"manage"},
	// The change feed spans every object in the tenant, so it is its own
	// grant rather than a consequence of the ones above (WP-2.1).
	"sync": {"read"},
}

// ErrTenantExists is returned when the tenant is already provisioned, so
// re-running bootstrap is a clear no-op rather than a confusing constraint
// violation from three layers down.
var ErrTenantExists = errors.New("app: tenant already exists")

// Bootstrap provisions the first tenant, its administrator, and the modules it
// starts with. Nothing in the product can create a tenant over HTTP — tenant
// provisioning is an operator action, not a self-service API — so without this
// a freshly migrated database has no way in at all.
//
// It runs entirely server-side under the operator's own database credentials
// and is deliberately not reachable over the API.
func Bootstrap(ctx context.Context, db *storage.DB, in BootstrapInput) error {
	if in.Tenant == "" || in.Email == "" || in.Password == "" {
		return errors.New("app: tenant, email and password are required")
	}

	if _, err := identity.GetUserByEmail(ctx, db, tenancy.ID(in.Tenant), in.Email); err == nil {
		return fmt.Errorf("%w: %s", ErrTenantExists, in.Tenant)
	}

	name := in.Name
	if name == "" {
		name = in.Tenant
	}
	tenant := tenancy.ID(in.Tenant)
	if err := tenancy.CreateTenant(ctx, db, tenant, name); err != nil {
		return fmt.Errorf("app: create tenant: %w", err)
	}

	hash, err := identity.HashPassword(in.Password)
	if err != nil {
		return fmt.Errorf("app: hash password: %w", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, in.Email, hash)
	if err != nil {
		return fmt.Errorf("app: create user: %w", err)
	}

	role, err := authz.CreateRole(ctx, db, tenant, "administrator", true)
	if err != nil {
		return fmt.Errorf("app: create role: %w", err)
	}
	for object, actions := range adminGrants {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, db, tenant, role, object, action, ""); err != nil {
				return fmt.Errorf("app: grant %s:%s: %w", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		return fmt.Errorf("app: assign role: %w", err)
	}

	modules := in.Modules
	if len(modules) == 0 {
		modules = defaultModules
	}
	reg, err := capability.Load()
	if err != nil {
		return fmt.Errorf("app: load capability registry: %w", err)
	}
	// capability.Enable authorizes against the calling actor, so the new admin
	// enables its own tenant's modules — the same path an operator would take
	// through the API, not a bypass.
	adminCtx := authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
	for _, module := range modules {
		if _, err := capability.Enable(adminCtx, db, reg, tenant, module); err != nil {
			return fmt.Errorf("app: enable %s: %w", module, err)
		}
	}
	return nil
}

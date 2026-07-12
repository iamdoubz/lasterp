package capability

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// AC: every shipped profile boots in CI. For each bootable profile,
// ApplyProfile against a real DB (both dialects) leaves EnabledModules equal
// to the solver's closure; the pending profile is refused.
func TestApplyProfileBootsEveryProfile(t *testing.T) {
	reg := mustLoad(t)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			for _, p := range Profiles() {
				if !p.Bootable() {
					ctx := authorizedCtx(t, db, mustCreateTenant(t, db))
					if _, err := ApplyProfile(ctx, db, reg, tenantOf(t, ctx), p); !errors.Is(err, ErrProfilePending) {
						t.Errorf("pending profile %s: got %v, want ErrProfilePending", p.Name, err)
					}
					continue
				}
				tenant := mustCreateTenant(t, db)
				ctx := authorizedCtx(t, db, tenant)
				closure, err := ApplyProfile(ctx, db, reg, tenant, p)
				if err != nil {
					t.Fatalf("apply %s: %v", p.Name, err)
				}
				enabled, err := EnabledModules(ctx, db, tenant)
				if err != nil {
					t.Fatalf("enabled after %s: %v", p.Name, err)
				}
				slices.Sort(enabled)
				if !slices.Equal(enabled, closure) {
					t.Errorf("profile %s: enabled %v, want closure %v", p.Name, enabled, closure)
				}
				if !slices.Contains(enabled, "kernel") {
					t.Errorf("profile %s did not enable kernel", p.Name)
				}
			}
		})
	}
}

func TestEnablePersistsClosureAndAudits(t *testing.T) {
	reg := mustLoad(t)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			ctx := authorizedCtx(t, db, tenant)

			res, err := Enable(ctx, db, reg, tenant, "invoicing")
			if err != nil {
				t.Fatalf("enable: %v", err)
			}
			if !slices.Contains(res.Added, "ledger") {
				t.Errorf("enable invoicing should pull ledger; added %v", res.Added)
			}
			enabled, _ := EnabledModules(ctx, db, tenant)
			for _, want := range []string{"invoicing", "contacts", "ledger", "tax-engine"} {
				if !slices.Contains(enabled, want) {
					t.Errorf("closure not persisted, missing %q in %v", want, enabled)
				}
			}
			// INV-T4: the change is audited.
			if n := auditCount(t, db, tenant, "module_state", "enable"); n != 1 {
				t.Errorf("enable audit rows = %d, want 1", n)
			}
			// Idempotent: re-enabling adds nothing.
			res2, _ := Enable(ctx, db, reg, tenant, "invoicing")
			if len(res2.Added) != 0 {
				t.Errorf("re-enable added %v, want none", res2.Added)
			}
		})
	}
}

// ADR-018 §5: disable retains the row (data), and re-enable restores it.
func TestDisableIsNotDeleteAndReenable(t *testing.T) {
	reg := mustLoad(t)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			ctx := authorizedCtx(t, db, tenant)

			if _, err := Enable(ctx, db, reg, tenant, "crm"); err != nil {
				t.Fatalf("enable crm: %v", err)
			}
			if err := Disable(ctx, db, reg, tenant, "crm"); err != nil {
				t.Fatalf("disable crm: %v", err)
			}
			if on, _ := IsModuleEnabled(ctx, db, tenant, "crm"); on {
				t.Error("crm still enabled after disable")
			}
			// Row retained (disable != delete): enabled=false row exists.
			if got := rowState(t, db, tenant, "crm"); got != "false" {
				t.Errorf("crm row after disable = %q, want retained-and-false", got)
			}
			if _, err := Enable(ctx, db, reg, tenant, "crm"); err != nil {
				t.Fatalf("re-enable crm: %v", err)
			}
			if on, _ := IsModuleEnabled(ctx, db, tenant, "crm"); !on {
				t.Error("crm not restored after re-enable")
			}
		})
	}
}

func TestDisableRefusedWhenInUse(t *testing.T) {
	reg := mustLoad(t)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			ctx := authorizedCtx(t, db, tenant)
			if _, err := Enable(ctx, db, reg, tenant, "invoicing"); err != nil {
				t.Fatalf("enable invoicing: %v", err)
			}
			err := Disable(ctx, db, reg, tenant, "ledger")
			var inUse ErrModuleInUse
			if !errors.As(err, &inUse) {
				t.Fatalf("disable ledger: got %v, want ErrModuleInUse", err)
			}
			if !slices.Contains(inUse.DependentModules, "invoicing") {
				t.Errorf("ErrModuleInUse should list invoicing; got %v", inUse.DependentModules)
			}
			// Refused ⇒ ledger stays on.
			if on, _ := IsModuleEnabled(ctx, db, tenant, "ledger"); !on {
				t.Error("ledger disabled despite in-use refusal")
			}
		})
	}
}

// INV-T2: no capability change without an authorized actor.
func TestChangeRequiresAuthz(t *testing.T) {
	reg := mustLoad(t)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			// No actor bound to the context.
			if _, err := Enable(context.Background(), db, reg, tenant, "crm"); !errors.Is(err, authz.ErrNoActor) {
				t.Errorf("enable without actor: got %v, want ErrNoActor", err)
			}
		})
	}
}

// INV-T1: one tenant's enable-state is invisible to another (RLS on Postgres).
func TestModuleStateTenantIsolation(t *testing.T) {
	reg := mustLoad(t)
	db, ok := testDialects(t)["postgres"]
	if !ok {
		t.Skip("postgres-only: SQLite solo mode is one tenant per replica (ADR-005)")
	}
	tenantA := mustCreateTenant(t, db)
	tenantB := mustCreateTenant(t, db)
	if _, err := ApplyProfile(authorizedCtx(t, db, tenantA), db, reg, tenantA, mustProfile(t, reg, "crm_only")); err != nil {
		t.Fatalf("apply to A: %v", err)
	}
	bEnabled, err := EnabledModules(authorizedCtx(t, db, tenantB), db, tenantB)
	if err != nil {
		t.Fatalf("enabled B: %v", err)
	}
	if len(bEnabled) != 0 {
		t.Errorf("tenant B sees %v, want none (cross-tenant leak)", bEnabled)
	}
}

// --- small test helpers ---

func tenantOf(t *testing.T, ctx context.Context) tenancy.ID {
	t.Helper()
	a, err := authz.ActorFromContext(ctx)
	if err != nil {
		t.Fatalf("actor from ctx: %v", err)
	}
	return a.TenantID
}

func mustProfile(t *testing.T, reg *Registry, name string) Profile {
	t.Helper()
	p, ok := reg.Profile(name)
	if !ok {
		t.Fatalf("profile %q missing", name)
	}
	return p
}

func auditCount(t *testing.T, db *storage.DB, tenant tenancy.ID, object, action string) int {
	t.Helper()
	var n int
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND object = ? AND action = ?`),
			string(tenant), object, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// rowState returns "missing", "true", or "false" for a module's stored row.
func rowState(t *testing.T, db *storage.DB, tenant tenancy.ID, module string) string {
	t.Helper()
	var enabled bool
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT enabled FROM module_state WHERE tenant_id = ? AND module = ?`),
			string(tenant), module).Scan(&enabled)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "missing"
	}
	if err != nil {
		t.Fatalf("row state: %v", err)
	}
	if enabled {
		return "true"
	}
	return "false"
}

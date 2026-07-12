package capability

import (
	"context"
	"testing"
)

// testRegistry builds a registry from inline manifests (white-box) so the
// adapter can be tested against an object→module binding the built-in catalog
// doesn't declare yet.
func testRegistry(t *testing.T, yamls ...string) *Registry {
	t.Helper()
	r := &Registry{modules: map[string]*Manifest{}, providers: map[string]string{}, objects: map[string]string{}}
	for _, y := range yamls {
		m, err := ParseManifest([]byte(y))
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		if err := r.add(m); err != nil {
			t.Fatalf("add manifest: %v", err)
		}
	}
	if err := r.validateGraph(); err != nil {
		t.Fatalf("validate graph: %v", err)
	}
	return r
}

// GatewayChecker maps an object to its module's enable-state: unowned objects
// are always enabled; an owned object is enabled iff its module is.
func TestGatewayCheckerAdapter(t *testing.T) {
	reg := testRegistry(t,
		"module: kernel\nkernel: true\nprovides: [documents]\n",
		"module: contacts\nprovides: [contacts]\n",
		"module: crm\nprovides: [crm]\nrequires: [contacts]\nobjects: [lead]\n",
	)
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			chk := GatewayChecker{Reg: reg, DB: db}
			ctx := context.Background()

			// Unowned (kernel) object: always reachable.
			if on, _, err := chk.Enabled(ctx, tenant, "some_kernel_object"); err != nil || !on {
				t.Errorf("unowned object: on=%v err=%v, want enabled", on, err)
			}
			// crm's object while crm is off.
			on, capName, err := chk.Enabled(ctx, tenant, "lead")
			if err != nil {
				t.Fatalf("check lead (disabled): %v", err)
			}
			if on {
				t.Error("lead should be disabled before crm is enabled")
			}
			if capName != "crm" {
				t.Errorf("disabled capability = %q, want crm", capName)
			}
			// Enable crm, then its object is reachable.
			if _, err := Enable(authorizedCtx(t, db, tenant), db, reg, tenant, "crm"); err != nil {
				t.Fatalf("enable crm: %v", err)
			}
			if on, _, err := chk.Enabled(ctx, tenant, "lead"); err != nil || !on {
				t.Errorf("lead after enabling crm: on=%v err=%v, want enabled", on, err)
			}
		})
	}
}

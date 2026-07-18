// SPDX-License-Identifier: AGPL-3.0-only

package contacts

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// TestCreateContact covers the one bit of module logic — the kind guard — and
// that a valid contact round-trips through the CRUD engine on SQLite.
func TestCreateContact(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "contacts.db") + "?_pragma=busy_timeout(30000)"
	db, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Register(ctx, db); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tenant := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(ctx, db, tenant, "t"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	hash, _ := identity.HashPassword("s3cret!")
	user, err := identity.CreateUser(ctx, db, tenant, "u@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "r", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, a := range []string{"create", "read"} {
		if err := authz.GrantPermission(ctx, db, tenant, role, ObjectContact, a, ""); err != nil {
			t.Fatalf("GrantPermission: %v", err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	actorCtx := authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})

	if _, err := CreateContact(actorCtx, db, tenant, "Acme", "a@acme.example", "not-a-kind"); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("invalid kind: err = %v, want ErrInvalidKind", err)
	}
	rec, err := CreateContact(actorCtx, db, tenant, "Acme", "a@acme.example", KindCustomer)
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if rec["name"] != "Acme" || rec["kind"] != KindCustomer {
		t.Fatalf("stored contact = %+v", rec)
	}
}

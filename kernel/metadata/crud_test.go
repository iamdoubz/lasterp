package metadata

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// buildContactCRUD parses+merges the sample Contact object (with one
// tenant overlay adding a field, proving overlay merge feeds all the way
// through to DDL/CRUD), applies its DDL, and returns the CRUD engine.
func buildContactCRUD(t *testing.T, db *storage.DB, tenant tenancy.ID) *CRUD {
	t.Helper()
	ctx := context.Background()
	core := sampleCore(t)
	overlay := Overlay{Layer: "tenant", AddFields: []Field{{Name: "vip", Type: FieldBool}}}
	eff, err := Merge(core, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := ApplyDDL(ctx, db, eff, 1); err != nil {
		t.Fatalf("ApplyDDL: %v", err)
	}
	crud, err := NewCRUD(eff)
	if err != nil {
		t.Fatalf("NewCRUD: %v", err)
	}
	return crud
}

// actorWithPermissions creates a user, a role granting the given actions
// on "Contact", assigns it, and returns a context bound with that actor.
func actorWithPermissions(t *testing.T, db *storage.DB, tenant tenancy.ID, actions ...string) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, "actor@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "contact-manager", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range actions {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Contact", action, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", action, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

// TestContactConformance is the WP-0.5 AC's proof case: define a sample
// object in YAML, generate its API (kernel/metadata.CRUD), and exercise
// it end to end.
func TestContactConformance(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := buildContactCRUD(t, db, tenant)
			ctx := actorWithPermissions(t, db, tenant, "create", "read", "update", "delete")

			created, err := crud.Create(ctx, db, tenant, Record{
				"full_name": "Ada Lovelace", "email": "ada@example.com", "vip": true,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id, _ := created["id"].(string)
			if id == "" {
				t.Fatal("Create did not assign an id")
			}

			got, err := crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got["full_name"] != "Ada Lovelace" {
				t.Fatalf("Get full_name = %v, want Ada Lovelace", got["full_name"])
			}
			if got["vip"] != true {
				t.Fatalf("Get vip (overlay field) = %v, want true", got["vip"])
			}

			list, err := crud.List(ctx, db, tenant)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("List returned %d records, want 1", len(list))
			}

			updated, err := crud.Update(ctx, db, tenant, id, Record{"full_name": "Ada, Countess of Lovelace"})
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if updated["full_name"] != "Ada, Countess of Lovelace" {
				t.Fatalf("Update full_name = %v", updated["full_name"])
			}

			if err := crud.SoftDelete(ctx, db, tenant, id); err != nil {
				t.Fatalf("SoftDelete: %v", err)
			}
			if _, err := crud.Get(ctx, db, tenant, id); err != nil {
				t.Fatalf("Get after soft-delete (row must still exist): %v", err)
			}
			list, err = crud.List(ctx, db, tenant)
			if err != nil {
				t.Fatalf("List after soft-delete: %v", err)
			}
			if len(list) != 0 {
				t.Fatalf("List after soft-delete returned %d records, want 0 (archived excluded)", len(list))
			}

			assertAuditActions(t, db, tenant, "Contact", id, []string{"create", "update", "delete"})
		})
	}
}

func TestContactCreateRequiresRequiredField(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := buildContactCRUD(t, db, tenant)
			ctx := actorWithPermissions(t, db, tenant, "create")

			_, err := crud.Create(ctx, db, tenant, Record{"email": "no-name@example.com"})
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation (full_name is required)", err)
			}
		})
	}
}

// INV-T2: no write without an authorization decision.
func TestContactCreateRequiresPermission(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := buildContactCRUD(t, db, tenant)
			ctx := actorWithPermissions(t, db, tenant /* no actions granted */)

			_, err := crud.Create(ctx, db, tenant, Record{"full_name": "Nobody"})
			if !errors.Is(err, authz.ErrPermissionDenied) {
				t.Fatalf("err = %v, want authz.ErrPermissionDenied", err)
			}
		})
	}
}

// INV-T1: a record created under one tenant is invisible to another.
func TestContactTenantIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)
			crudA := buildContactCRUD(t, db, tenantA)
			ctxA := actorWithPermissions(t, db, tenantA, "create")

			created, err := crudA.Create(ctxA, db, tenantA, Record{"full_name": "Tenant A's Contact"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id, _ := created["id"].(string)

			crudB := buildContactCRUD(t, db, tenantB)
			ctxB := actorWithPermissions(t, db, tenantB, "read")
			if _, err := crudB.Get(ctxB, db, tenantB, id); !errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("tenant B Get tenant A's record: err = %v, want ErrRecordNotFound", err)
			}
		})
	}
}

// assertAuditActions reads audit_log through tenancy.WithTenant — a bare
// db.QueryContext would run with no tenant context set and RLS would
// silently return zero rows (the WP-0.4 lesson).
func assertAuditActions(t *testing.T, db *storage.DB, tenant tenancy.ID, object, recordID string, wantActions []string) {
	t.Helper()
	ctx := context.Background()

	var gotActions []string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT action FROM audit_log WHERE tenant_id = ? AND object = ? AND record_id = ? ORDER BY at ASC`),
			string(tenant), object, recordID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var action string
			if err := rows.Scan(&action); err != nil {
				return err
			}
			gotActions = append(gotActions, action)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}

	if len(gotActions) != len(wantActions) {
		t.Fatalf("audit actions = %v, want %v", gotActions, wantActions)
	}
	for i, a := range wantActions {
		if gotActions[i] != a {
			t.Fatalf("audit actions = %v, want %v", gotActions, wantActions)
		}
	}
}

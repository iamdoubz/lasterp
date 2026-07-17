//go:build integrity

// WP-1.0a evolution integrity suite — joins the Integrity Gauntlet (docs/19
// §3 "Migration integrity": every schema migration round-trips on seeded data
// with pre/post invariant checks). Proves that evolving a *populated* object
// preserves its data on Postgres AND SQLite, that tenant isolation (INV-T1)
// survives the migration, and that a destructive diff is refused without
// touching the data.
package metadata

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// memberObject is a CRUD object with an int field (widened to decimal in v2)
// and a required text field (never widened).
func memberObject(fields ...Field) *Object {
	return &Object{
		ObjectName:  "Member",
		Module:      "test",
		Persistence: PersistenceCRUD,
		Fields:      fields,
		Permissions: Permissions{
			"read": {"member.role"}, "create": {"member.role"},
			"update": {"member.role"}, "delete": {"member.role"},
		},
	}
}

func memberV1(t *testing.T) *EffectiveSchema {
	return eff(t, memberObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "email", Type: FieldEmail},
		Field{Name: "points", Type: FieldInt},
	))
}

// memberV2 adds an optional field (phone) and widens points int→decimal.
func memberV2(t *testing.T) *EffectiveSchema {
	return eff(t, memberObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "email", Type: FieldEmail},
		Field{Name: "points", Type: FieldDecimal},
		Field{Name: "phone", Type: FieldText},
	))
}

// memberActor grants the given actions on "Member" and returns a bound ctx.
func memberActor(t *testing.T, db *storage.DB, tenant tenancy.ID, actions ...string) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, "member-actor@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "member-manager", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, a := range actions {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Member", a, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", a, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

// INV-T1 + migration integrity: add-field/widen-type on a populated object
// round-trips on Postgres AND SQLite, data intact.
func TestEvolvePopulatedObjectRoundTrips(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			v1 := memberV1(t)
			if err := ApplyDDL(ctx, db, v1, 1); err != nil {
				t.Fatalf("ApplyDDL v1: %v", err)
			}
			crud1, err := NewCRUD(v1)
			if err != nil {
				t.Fatalf("NewCRUD v1: %v", err)
			}
			actorCtx := memberActor(t, db, tenant, "create", "read", "update")

			ada, err := crud1.Create(actorCtx, db, tenant, Record{"name": "Ada", "email": "ada@example.com", "points": 100})
			if err != nil {
				t.Fatalf("Create Ada: %v", err)
			}
			if _, err := crud1.Create(actorCtx, db, tenant, Record{"name": "Grace", "email": "grace@example.com", "points": 250}); err != nil {
				t.Fatalf("Create Grace: %v", err)
			}
			adaID, _ := ada["id"].(string)

			// Evolve to v2: ADD COLUMN phone + widen points int→decimal.
			v2 := memberV2(t)
			if err := ApplyDDL(ctx, db, v2, 2); err != nil {
				t.Fatalf("ApplyDDL v2 (evolve): %v", err)
			}
			crud2, err := NewCRUD(v2)
			if err != nil {
				t.Fatalf("NewCRUD v2: %v", err)
			}

			// Old data intact under the new schema.
			got, err := crud2.Get(actorCtx, db, tenant, adaID)
			if err != nil {
				t.Fatalf("Get Ada after evolve: %v", err)
			}
			if got["name"] != "Ada" || got["email"] != "ada@example.com" {
				t.Fatalf("Ada core fields = %v, want name=Ada email=ada@example.com", got)
			}
			// points widened INT→TEXT: the integer 100 round-trips as "100"
			// on both dialects (Postgres via ::TEXT cast, SQLite via affinity).
			if got["points"] != "100" {
				t.Fatalf("Ada points after widen = %v (%T), want \"100\"", got["points"], got["points"])
			}
			// New column is NULL for pre-existing rows.
			if v, ok := got["phone"]; ok && v != nil {
				t.Fatalf("Ada phone = %v, want absent/nil on a pre-evolution row", v)
			}

			list, err := crud2.List(actorCtx, db, tenant)
			if err != nil {
				t.Fatalf("List after evolve: %v", err)
			}
			if len(list) != 2 {
				t.Fatalf("List after evolve = %d rows, want 2 (no data lost)", len(list))
			}

			// The evolved schema accepts writes using the new field. points is
			// now a decimal — a string-valued field (exact text, never a float),
			// so callers provide "42", not the int they'd have used pre-widening.
			neo, err := crud2.Create(actorCtx, db, tenant, Record{"name": "Neo", "points": "42", "phone": "+1-555-0100"})
			if err != nil {
				t.Fatalf("Create on evolved schema: %v", err)
			}
			neoGot, err := crud2.Get(actorCtx, db, tenant, neo["id"].(string))
			if err != nil {
				t.Fatalf("Get Neo: %v", err)
			}
			if neoGot["phone"] != "+1-555-0100" || neoGot["points"] != "42" {
				t.Fatalf("Neo = %v, want phone=+1-555-0100 points=42", neoGot)
			}

			// INV-T1: another tenant still sees none of tenant's evolved rows.
			other := mustCreateTenant(t, db)
			otherCtx := memberActor(t, db, other, "read")
			if _, err := crud2.Get(otherCtx, db, other, adaID); !errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("cross-tenant Get after evolve: err = %v, want ErrRecordNotFound", err)
			}
		})
	}
}

// AC2 at the storage layer: a destructive evolution is refused and leaves the
// populated table untouched (no partial apply).
func TestEvolveDestructiveDiffRefusedNoDataLoss(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			v1 := memberV1(t)
			if err := ApplyDDL(ctx, db, v1, 1); err != nil {
				t.Fatalf("ApplyDDL v1: %v", err)
			}
			crud1, err := NewCRUD(v1)
			if err != nil {
				t.Fatalf("NewCRUD v1: %v", err)
			}
			actorCtx := memberActor(t, db, tenant, "create", "read")
			ada, err := crud1.Create(actorCtx, db, tenant, Record{"name": "Ada", "email": "ada@example.com", "points": 100})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			adaID := ada["id"].(string)

			// v2 drops the email column — destructive.
			destructive := eff(t, memberObject(
				Field{Name: "name", Type: FieldText, Required: true},
				Field{Name: "points", Type: FieldInt},
			))
			if err := ApplyDDL(ctx, db, destructive, 2); !errors.Is(err, ErrDestructiveDiff) {
				t.Fatalf("ApplyDDL destructive: err = %v, want ErrDestructiveDiff", err)
			}

			// Nothing changed: v1 still reads the row with its email intact.
			got, err := crud1.Get(actorCtx, db, tenant, adaID)
			if err != nil {
				t.Fatalf("Get after refused evolve: %v", err)
			}
			if got["email"] != "ada@example.com" {
				t.Fatalf("email after refused evolve = %v, want ada@example.com (no partial apply)", got["email"])
			}
		})
	}
}

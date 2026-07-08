package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func mustCreateUser(t *testing.T, db *storage.DB, tenant tenancy.ID, email string) identity.UserID {
	t.Helper()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u, err := identity.CreateUser(context.Background(), db, tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func TestGrantAndRevokePermission(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			user := mustCreateUser(t, db, tenant, "grantee@example.com")

			role, err := CreateRole(ctx, db, tenant, "clerk", false)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			if err := AssignRole(ctx, db, tenant, user, role); err != nil {
				t.Fatalf("AssignRole: %v", err)
			}
			actor := Actor{TenantID: tenant, UserID: user}

			ok, err := Can(ctx, db, actor, "invoice", "read")
			if err != nil {
				t.Fatalf("Can (before grant): %v", err)
			}
			if ok {
				t.Fatal("Can returned true before any grant")
			}

			if err := GrantPermission(ctx, db, tenant, role, "invoice", "read", ""); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}
			ok, err = Can(ctx, db, actor, "invoice", "read")
			if err != nil {
				t.Fatalf("Can (after grant): %v", err)
			}
			if !ok {
				t.Fatal("Can returned false after grant")
			}

			if err := RevokePermission(ctx, db, tenant, role, "invoice", "read"); err != nil {
				t.Fatalf("RevokePermission: %v", err)
			}
			ok, err = Can(ctx, db, actor, "invoice", "read")
			if err != nil {
				t.Fatalf("Can (after revoke): %v", err)
			}
			if ok {
				t.Fatal("Can returned true after revoke")
			}
		})
	}
}

func TestGrantPermissionRejectsCondition(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			role, err := CreateRole(ctx, db, tenant, "clerk", false)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			err = GrantPermission(ctx, db, tenant, role, "invoice", "read", "record.owner == actor.id")
			if !errors.Is(err, ErrConditionNotSupported) {
				t.Fatalf("GrantPermission with condition: err = %v, want ErrConditionNotSupported", err)
			}
		})
	}
}

// INV-T3: permission floors cannot be lowered by the tenant-facing API.
func TestCorePermissionFloorCannotBeRevoked(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			role, err := CreateRole(ctx, db, tenant, "admin", true)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			if err := GrantPermission(ctx, db, tenant, role, "tenant", "delete", ""); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}

			err = RevokePermission(ctx, db, tenant, role, "tenant", "delete")
			if !errors.Is(err, ErrCorePermissionFloor) {
				t.Fatalf("RevokePermission on core role: err = %v, want ErrCorePermissionFloor", err)
			}
		})
	}
}

// INV-T4: no write path executes without an attributed actor.
func TestActorFromContextRequiresActor(t *testing.T) {
	if _, err := ActorFromContext(context.Background()); !errors.Is(err, ErrNoActor) {
		t.Fatalf("ActorFromContext on bare context: err = %v, want ErrNoActor", err)
	}

	ctx := WithActor(context.Background(), Actor{TenantID: "t1", UserID: "u1"})
	actor, err := ActorFromContext(ctx)
	if err != nil {
		t.Fatalf("ActorFromContext with bound actor: %v", err)
	}
	if actor.TenantID != "t1" || actor.UserID != "u1" {
		t.Fatalf("actor = %+v, want {t1 u1}", actor)
	}
}

// INV-T2: no write path executes without an authorization decision, and
// INV-T4 ties that decision to the attributed actor — Authorize enforces
// both together.
func TestAuthorizeRequiresActorAndGrant(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			user := mustCreateUser(t, db, tenant, "authorize@example.com")

			if _, err := Authorize(ctx, db, "invoice", "post"); !errors.Is(err, ErrNoActor) {
				t.Fatalf("Authorize with no actor: err = %v, want ErrNoActor", err)
			}

			actorCtx := WithActor(ctx, Actor{TenantID: tenant, UserID: user})
			if _, err := Authorize(actorCtx, db, "invoice", "post"); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("Authorize with actor but no grant: err = %v, want ErrPermissionDenied", err)
			}

			role, err := CreateRole(ctx, db, tenant, "poster", false)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			if err := AssignRole(ctx, db, tenant, user, role); err != nil {
				t.Fatalf("AssignRole: %v", err)
			}
			if err := GrantPermission(ctx, db, tenant, role, "invoice", "post", ""); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}

			actor, err := Authorize(actorCtx, db, "invoice", "post")
			if err != nil {
				t.Fatalf("Authorize after grant: %v", err)
			}
			if actor.UserID != user {
				t.Fatalf("Authorize returned actor %+v, want user %s", actor, user)
			}
		})
	}
}

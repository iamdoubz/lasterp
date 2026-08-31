// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// grantee sets up a tenant with one user in one non-core role.
func grantee(t *testing.T, db *storage.DB, email, roleName string) (tenancy.ID, identity.UserID, RoleID, Actor) {
	t.Helper()
	ctx := context.Background()
	tenant := mustCreateTenant(t, db)
	user := mustCreateUser(t, db, tenant, email)
	role, err := CreateRole(ctx, db, tenant, roleName, false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return tenant, user, role, Actor{TenantID: tenant, UserID: user}
}

func TestGrantPermissionStoresAndHonoursACondition(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, user, role, actor := grantee(t, db, "owner@example.com", "clerk")
			if err := GrantPermission(ctx, db, tenant, role, "invoice", ActionUpdate, `record.owner == actor.id`); err != nil {
				t.Fatalf("GrantPermission with condition: %v", err)
			}
			ctx = WithActor(ctx, actor)

			mine := map[string]any{"owner": string(user)}
			if _, err := AuthorizeRecord(ctx, db, "invoice", ActionUpdate, mine); err != nil {
				t.Fatalf("AuthorizeRecord on an owned record: %v", err)
			}
			theirs := map[string]any{"owner": "someone-else"}
			if _, err := AuthorizeRecord(ctx, db, "invoice", ActionUpdate, theirs); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("AuthorizeRecord on someone else's record: err = %v, want ErrPermissionDenied", err)
			}
		})
	}
}

// A condition may read the actor's role names (docs/08's own example needs a
// fact about the actor beyond the id).
func TestConditionCanReadActorRoles(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, _, role, actor := grantee(t, db, "clerk@example.com", "clerk")
			if err := GrantPermission(ctx, db, tenant, role, "invoice", ActionUpdate, `"clerk" in actor.roles`); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}
			ctx = WithActor(ctx, actor)
			if _, err := AuthorizeRecord(ctx, db, "invoice", ActionUpdate, nil); err != nil {
				t.Fatalf("AuthorizeRecord for a role the actor holds: %v", err)
			}

			tenant2, _, role2, actor2 := grantee(t, db, "other@example.com", "auditor")
			if err := GrantPermission(ctx, db, tenant2, role2, "invoice", ActionUpdate, `"clerk" in actor.roles`); err != nil {
				t.Fatalf("GrantPermission (second tenant): %v", err)
			}
			ctx2 := WithActor(context.Background(), actor2)
			if _, err := AuthorizeRecord(ctx2, db, "invoice", ActionUpdate, nil); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("AuthorizeRecord for a role the actor lacks: err = %v, want ErrPermissionDenied", err)
			}
		})
	}
}

// Refused rather than stored and ignored: a rule the gate never consults is a
// permission that reads as narrower than it is. The error names the WP that
// owns the missing half.
func TestGrantPermissionRefusesAConditionOnAnUnwiredAction(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, _, role, _ := grantee(t, db, "reader@example.com", "clerk")
			for _, action := range []string{ActionRead, "post", "manage"} {
				err := GrantPermission(ctx, db, tenant, role, "invoice", action, `record.owner == actor.id`)
				if !errors.Is(err, ErrConditionUnsupportedAction) {
					t.Fatalf("GrantPermission(condition on %q): err = %v, want ErrConditionUnsupportedAction", action, err)
				}
				if !strings.Contains(err.Error(), "lands with") {
					t.Fatalf("refusal for %q does not name the WP that owns it: %v", action, err)
				}
			}
		})
	}
}

// A condition is compiled at grant time so a mistyped rule fails where the
// administrator can see it, instead of denying every request forever.
func TestGrantPermissionRefusesAnInvalidCondition(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, _, role, actor := grantee(t, db, "typo@example.com", "clerk")
			for _, cond := range []string{
				`record.owner ==`,          // malformed
				`record.owner`,             // not a boolean
				`request.headers["x"] > 1`, // outside the environment
			} {
				if err := GrantPermission(ctx, db, tenant, role, "invoice", ActionUpdate, cond); !errors.Is(err, ErrConditionInvalid) {
					t.Fatalf("GrantPermission(%q): err = %v, want ErrConditionInvalid", cond, err)
				}
			}
			// And nothing was stored: the actor holds no grant at all.
			ctx = WithActor(ctx, actor)
			if _, err := AuthorizeRecord(ctx, db, "invoice", ActionUpdate, map[string]any{"owner": string(actor.UserID)}); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("a refused grant left something behind: err = %v, want ErrPermissionDenied", err)
			}
		})
	}
}

// An unconditional grant is unchanged in every way by WP-3.3a — this is the
// path every existing caller is on.
func TestUnconditionalGrantIsUnchanged(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, _, role, actor := grantee(t, db, "plain@example.com", "clerk")
			if err := GrantPermission(ctx, db, tenant, role, "invoice", ActionUpdate, ""); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}
			ok, err := Can(ctx, db, actor, "invoice", ActionUpdate)
			if err != nil || !ok {
				t.Fatalf("Can on an unconditional grant: ok = %v, err = %v", ok, err)
			}
			objects, err := GrantedObjects(ctx, db, actor, ActionUpdate)
			if err != nil {
				t.Fatalf("GrantedObjects: %v", err)
			}
			if len(objects) != 1 || objects[0] != "invoice" {
				t.Fatalf("GrantedObjects = %v, want [invoice]", objects)
			}
			ctx = WithActor(ctx, actor)
			// No record, no condition: AuthorizeRecord must still allow.
			if _, err := AuthorizeRecord(ctx, db, "invoice", ActionUpdate, nil); err != nil {
				t.Fatalf("AuthorizeRecord on an unconditional grant: %v", err)
			}
		})
	}
}

func TestGrantsForRequiresAnActor(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			if _, err := GrantsFor(context.Background(), db, Actor{}, "invoice", ActionUpdate); !errors.Is(err, ErrNoActor) {
				t.Fatalf("GrantsFor with no actor: err = %v, want ErrNoActor", err)
			}
		})
	}
}

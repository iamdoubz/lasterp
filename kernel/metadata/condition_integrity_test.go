//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

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

// The condition every case here uses: docs/08 §AuthZ's own example. It reads an
// *overlay* field, which is the shape a tenant's own row-level rule actually
// has under ADR-006 — the custom field is where "owner" lives for a core object.
const ownerRule = `record.owner == actor.id`

// owned is one tenant with an owner-bearing Contact object and two actors on
// it: `cond`, whose write grants carry ownerRule, and `plain`, whose identical
// grants carry no condition.
//
// The plain actor is not scaffolding — it is the control arm. Every denial
// below is paired with the same call succeeding for `plain`, so a red test
// means the condition denied, not that the row, the schema or the fixture was
// wrong.
type owned struct {
	tenant tenancy.ID
	crud   *CRUD
	cond   context.Context
	plain  context.Context
	me     string // the cond actor's user id
}

func setupOwned(t *testing.T, db *storage.DB) owned {
	t.Helper()
	tenant := mustCreateTenant(t, db)
	eff, err := Merge(sampleCore(t), Overlay{Layer: "tenant", AddFields: []Field{{Name: "owner", Type: FieldText}}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := ApplyDDL(context.Background(), db, eff, 1); err != nil {
		t.Fatalf("ApplyDDL: %v", err)
	}
	crud, err := NewCRUD(eff)
	if err != nil {
		t.Fatalf("NewCRUD: %v", err)
	}
	cond, me := writerWithCondition(t, db, tenant, "scoped@example.com", ownerRule)
	plain, _ := writerWithCondition(t, db, tenant, "unscoped@example.com", "")
	return owned{tenant: tenant, crud: crud, cond: cond, plain: plain, me: me}
}

// writerWithCondition grants create/update/delete on Contact carrying
// condition (pass "" for an unconditional grant) plus an unconditional read,
// and returns the bound context and the user id.
//
// Read is unconditional in both arms because a conditional read grant is
// refused at grant time (WP-3.3-decisions.md §3) — and because it is what lets
// the test observe what the write path did.
func writerWithCondition(t *testing.T, db *storage.DB, tenant tenancy.ID, email, condition string) (context.Context, string) {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "writer-"+email, false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range []string{authz.ActionCreate, authz.ActionUpdate, authz.ActionDelete} {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Contact", action, condition); err != nil {
			t.Fatalf("GrantPermission(%s, %q): %v", action, condition, err)
		}
	}
	if err := authz.GrantPermission(ctx, db, tenant, role, "Contact", authz.ActionRead, ""); err != nil {
		t.Fatalf("GrantPermission(read): %v", err)
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID}), string(user.ID)
}

// INV-T2/INV-T3: a conditional grant narrows the real write pipeline on every
// verb that carries one, and a denial writes nothing at all.
func TestConditionNarrowsEveryWriteVerb(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			t.Run("create", func(t *testing.T) { assertCreateNarrowed(t, db) })
			t.Run("update", func(t *testing.T) { assertUpdateNarrowed(t, db) })
			t.Run("delete", func(t *testing.T) { assertDeleteNarrowed(t, db) })
		})
	}
}

func assertCreateNarrowed(t *testing.T, db *storage.DB) {
	t.Helper()
	o := setupOwned(t, db)

	if _, err := o.crud.Create(o.cond, db, o.tenant, Record{"full_name": "Mine", "owner": o.me}); err != nil {
		t.Fatalf("Create of a record I own: %v", err)
	}
	_, err := o.crud.Create(o.cond, db, o.tenant, Record{"full_name": "Theirs", "owner": "someone-else"})
	if !errors.Is(err, authz.ErrPermissionDenied) {
		t.Fatalf("Create of a record owned by someone else: err = %v, want ErrPermissionDenied", err)
	}
	// Denied means nothing was written, not "written and then reported".
	for _, rec := range mustList(t, o.plain, o.crud, db, o.tenant) {
		if rec["full_name"] == "Theirs" {
			t.Fatal("the denied Create wrote its row anyway")
		}
	}
	// Control arm: the identical create, unconditionally granted, succeeds.
	if _, err := o.crud.Create(o.plain, db, o.tenant, Record{"full_name": "Theirs", "owner": "someone-else"}); err != nil {
		t.Fatalf("the unconditional control arm also refused the create: %v — the denial above proves nothing about the condition", err)
	}
}

func assertUpdateNarrowed(t *testing.T, db *storage.DB) {
	t.Helper()
	o := setupOwned(t, db)
	mine := mustCreate(t, o.cond, o.crud, db, o.tenant, Record{"full_name": "Mine", "owner": o.me})
	theirs := mustCreate(t, o.plain, o.crud, db, o.tenant, Record{"full_name": "Theirs", "owner": "someone-else"})

	if _, err := o.crud.Update(o.cond, db, o.tenant, mine, Record{"full_name": "Mine, renamed"}); err != nil {
		t.Fatalf("Update of a record I own: %v", err)
	}
	if _, err := o.crud.Update(o.cond, db, o.tenant, theirs, Record{"full_name": "Hijacked"}); !errors.Is(err, authz.ErrPermissionDenied) {
		t.Fatalf("Update of someone else's record: err = %v, want ErrPermissionDenied", err)
	}
	got, err := o.crud.Get(o.plain, db, o.tenant, theirs)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["full_name"] != "Theirs" {
		t.Fatalf("the denied Update changed the row anyway: full_name = %v", got["full_name"])
	}
	if _, err := o.crud.Update(o.plain, db, o.tenant, theirs, Record{"full_name": "Hijacked"}); err != nil {
		t.Fatalf("the unconditional control arm also refused the update: %v", err)
	}
}

func assertDeleteNarrowed(t *testing.T, db *storage.DB) {
	t.Helper()
	o := setupOwned(t, db)
	mine := mustCreate(t, o.cond, o.crud, db, o.tenant, Record{"full_name": "Mine", "owner": o.me})
	theirs := mustCreate(t, o.plain, o.crud, db, o.tenant, Record{"full_name": "Theirs", "owner": "someone-else"})

	if err := o.crud.SoftDelete(o.cond, db, o.tenant, mine); err != nil {
		t.Fatalf("SoftDelete of a record I own: %v", err)
	}
	if err := o.crud.SoftDelete(o.cond, db, o.tenant, theirs); !errors.Is(err, authz.ErrPermissionDenied) {
		t.Fatalf("SoftDelete of someone else's record: err = %v, want ErrPermissionDenied", err)
	}
	got, err := o.crud.Get(o.plain, db, o.tenant, theirs)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("the denied SoftDelete archived the row anyway")
	}
	if err := o.crud.SoftDelete(o.plain, db, o.tenant, theirs); err != nil {
		t.Fatalf("the unconditional control arm also refused the delete: %v", err)
	}
}

// A conditional grant on read is refused at grant time rather than stored and
// ignored: GrantedObjects and CRUD.List answer object-level questions and have
// nowhere to put a row predicate (WP-3.3-decisions.md §3).
func TestConditionalReadGrantIsRefusedAtTheMetadataLayer(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			role, err := authz.CreateRole(ctx, db, tenant, "reader", false)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			err = authz.GrantPermission(ctx, db, tenant, role, "Contact", authz.ActionRead, ownerRule)
			if !errors.Is(err, authz.ErrConditionUnsupportedAction) {
				t.Fatalf("GrantPermission(read, condition): err = %v, want ErrConditionUnsupportedAction", err)
			}
		})
	}
}

func mustCreate(t *testing.T, ctx context.Context, crud *CRUD, db *storage.DB, tenant tenancy.ID, rec Record) string {
	t.Helper()
	out, err := crud.Create(ctx, db, tenant, rec)
	if err != nil {
		t.Fatalf("Create(%v): %v", rec, err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("Create returned no id: %v", out)
	}
	return id
}

func mustList(t *testing.T, ctx context.Context, crud *CRUD, db *storage.DB, tenant tenancy.ID) []Record {
	t.Helper()
	recs, err := crud.List(ctx, db, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return recs
}

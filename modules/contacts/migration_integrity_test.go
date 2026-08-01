//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package contacts

import (
	"context"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// docs/19, "Migration integrity": every schema migration round-trips on seeded
// data with pre/post checks. WP-1.7 is the first WP to evolve a *shipped*
// module's object (Contact v1 → v2, adding locale), so this proves the upgrade
// path on a populated database rather than a fresh one — which is the only case
// that can lose data, and the case no fresh-boot test ever exercises.

// contactV1YAML is the definition as it shipped before WP-1.7. It is a frozen
// copy on purpose: the test is about what happens to a database created by the
// old code, so it must not follow the current schema around.
//
// One deliberate deviation from the v1 bytes: `kind` carries its `options`.
// WP-1.11 made options mandatory on an enum, so the literal v1 text no longer
// parses — but the closed set {customer, vendor, both} was not *new* in
// WP-1.11, it existed the whole time in this module's validKinds map. WP-1.11
// moved where it is written down, not what it is. Declaring it here therefore
// changes nothing about what v1 meant, and the frozen-ness that this test
// depends on — v1 has no `locale` field — is untouched.
//
// The upgrade path itself never re-parses old YAML: Register() parses the
// current definition and ApplyDDL diffs against the JSON snapshot in
// object_schema_migrations. So a real pre-WP-1.11 database upgrades cleanly;
// it is only this test's simulation of the old world, built with today's
// parser, that needed the adjustment.
const contactV1YAML = `
object: Contact
module: contacts
persistence: crud
fields:
  - {name: name, type: text, required: true, index: true}
  - {name: email, type: email}
  - {name: kind, type: enum, required: true, options: [customer, vendor, both]}
permissions:
  read: [contacts.viewer]
  create: [contacts.admin]
  update: [contacts.admin]
  delete: [contacts.admin]
`

func TestContactSchemaEvolvesWithDataIntact(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			// --- the world as it was: v1 schema, one stored contact ---
			v1, err := metadata.ParseObject([]byte(contactV1YAML))
			if err != nil {
				t.Fatalf("parse v1: %v", err)
			}
			effV1, err := metadata.Merge(v1)
			if err != nil {
				t.Fatalf("merge v1: %v", err)
			}
			if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, ObjectContact, 1, []byte(contactV1YAML)); err != nil {
				t.Fatalf("save v1 schema: %v", err)
			}
			if err := metadata.ApplyDDL(ctx, db, effV1, 1); err != nil {
				t.Fatalf("apply v1 DDL: %v", err)
			}

			actorCtx := grantContactActor(t, db, tenant)
			crudV1, err := metadata.NewCRUD(effV1)
			if err != nil {
				t.Fatalf("NewCRUD v1: %v", err)
			}
			existing, err := crudV1.Create(actorCtx, db, tenant, metadata.Record{
				"name": "Acme GmbH", "email": "ap@acme.example", "kind": KindCustomer,
			})
			if err != nil {
				t.Fatalf("create v1 contact: %v", err)
			}
			existingID, _ := existing["id"].(string)

			// --- the upgrade: booting the new code registers v2 ---
			if err := Register(ctx, db); err != nil {
				t.Fatalf("Register (v1 → v2): %v", err)
			}

			// Pre-existing data survives, with the new field simply absent.
			after, err := crudCurrent(t).Get(actorCtx, db, tenant, existingID)
			if err != nil {
				t.Fatalf("get pre-existing contact after upgrade: %v", err)
			}
			if after["name"] != "Acme GmbH" || after["email"] != "ap@acme.example" {
				t.Errorf("upgrade altered stored data: %v", after)
			}
			if locale, _ := after["locale"].(string); locale != "" {
				t.Errorf("pre-existing contact gained a locale %q from nowhere", locale)
			}

			// And the new column is usable.
			created, err := crudCurrent(t).Create(actorCtx, db, tenant, metadata.Record{
				"name": "Kojote GmbH", "email": "de@acme.example", "kind": KindCustomer, "locale": "de",
			})
			if err != nil {
				t.Fatalf("create contact with a locale: %v", err)
			}
			locale, err := LocaleOf(actorCtx, db, tenant, created["id"].(string))
			if err != nil {
				t.Fatalf("LocaleOf: %v", err)
			}
			if locale != "de" {
				t.Errorf("LocaleOf = %q, want %q", locale, "de")
			}

			// Re-registering (every boot does) stays a no-op.
			if err := Register(ctx, db); err != nil {
				t.Fatalf("second Register: %v", err)
			}
		})
	}
}

func crudCurrent(t *testing.T) *metadata.CRUD {
	t.Helper()
	crud, err := contactCRUD()
	if err != nil {
		t.Fatalf("contactCRUD: %v", err)
	}
	return crud
}

func grantContactActor(t *testing.T, db *storage.DB, tenant tenancy.ID) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, "contacts@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "contacts-admin", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range []string{"create", "read", "update", "delete"} {
		if err := authz.GrantPermission(ctx, db, tenant, role, ObjectContact, action, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", action, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

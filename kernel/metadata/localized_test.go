package metadata

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/i18n"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"golang.org/x/text/language"
)

// An Item is the shape docs/17 names directly: "designated fields (item names,
// descriptions) support per-locale values".
const localizedItemYAML = `
object: Item
module: inventory
persistence: crud
fields:
  - {name: name, type: text, required: true, localized: true}
  - {name: notes, type: long_text, localized: true}
  - {name: sku, type: text}
permissions:
  read: [inventory.viewer]
  create: [inventory.admin]
  update: [inventory.admin]
  delete: [inventory.admin]
`

func buildItemCRUD(t *testing.T, db *storage.DB) *CRUD {
	t.Helper()
	obj, err := ParseObject([]byte(localizedItemYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	eff, err := Merge(obj)
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
	return crud
}

func itemActor(t *testing.T, db *storage.DB, tenant tenancy.ID) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, "item-actor@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "item-manager", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range []string{"create", "read", "update", "delete"} {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Item", action, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", action, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

// Per-locale values must survive the full round trip on both dialects: they
// live in the custom_fields blob, which is re-marshalled on every update.
func TestLocalizedFieldRoundTrip(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := buildItemCRUD(t, db)
			ctx := itemActor(t, db, tenant)

			created, err := crud.Create(ctx, db, tenant, Record{
				"name": "Rocket-powered roller skates",
				"sku":  "ACME-1",
				TranslationsKey: Translations{
					"name": {"de": "Raketenrollschuhe", "es": "Patines cohete"},
				},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id := created["id"].(string)
			assertTranslation(t, created, "name", "de", "Raketenrollschuhe")

			got, err := crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			assertTranslation(t, got, "name", "de", "Raketenrollschuhe")
			assertTranslation(t, got, "name", "es", "Patines cohete")
			if got["name"] != "Rocket-powered roller skates" {
				t.Errorf("canonical name = %v, want the untranslated value", got["name"])
			}

			// An update that never mentions translations must not lose them.
			if _, err := crud.Update(ctx, db, tenant, id, Record{"sku": "ACME-2"}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			got, err = crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get after unrelated update: %v", err)
			}
			assertTranslation(t, got, "name", "de", "Raketenrollschuhe")

			// Supplying translations replaces the set wholesale.
			if _, err := crud.Update(ctx, db, tenant, id, Record{
				TranslationsKey: Translations{"notes": {"de": "Vorsicht: sehr schnell"}},
			}); err != nil {
				t.Fatalf("Update translations: %v", err)
			}
			got, err = crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get after translation update: %v", err)
			}
			assertTranslation(t, got, "notes", "de", "Vorsicht: sehr schnell")
			if translations, ok := got[TranslationsKey].(Translations); ok {
				if _, stillThere := translations["name"]; stillThere {
					t.Error("replacing translations kept the previous field's values")
				}
			}

			// And an empty set clears the key entirely rather than leaving an
			// empty object on every record.
			if _, err := crud.Update(ctx, db, tenant, id, Record{TranslationsKey: Translations{}}); err != nil {
				t.Fatalf("Update clearing translations: %v", err)
			}
			got, err = crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get after clearing: %v", err)
			}
			if _, present := got[TranslationsKey]; present {
				t.Errorf("cleared record still carries %q: %v", TranslationsKey, got[TranslationsKey])
			}
		})
	}
}

// The API boundary hands over decoded JSON, not Go types.
func TestLocalizedAcceptsDecodedJSON(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	crud := buildItemCRUD(t, db)
	ctx := itemActor(t, db, tenant)

	created, err := crud.Create(ctx, db, tenant, Record{
		"name": "Anvil",
		TranslationsKey: map[string]any{
			"name": map[string]any{"de": "Amboss"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := crud.Get(ctx, db, tenant, created["id"].(string))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertTranslation(t, got, "name", "de", "Amboss")
}

// Translating a field nobody declared localized is refused: silently storing it
// would leave the caller believing the object is translated when no read path
// will ever look.
func TestTranslationsForUndeclaredFieldRejected(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	crud := buildItemCRUD(t, db)
	ctx := itemActor(t, db, tenant)

	_, err := crud.Create(ctx, db, tenant, Record{
		"name":          "Anvil",
		TranslationsKey: Translations{"sku": {"de": "Amboss-42"}},
	})
	if !errors.Is(err, ErrNotLocalized) {
		t.Fatalf("Create with translations for a non-localized field: err = %v, want ErrNotLocalized", err)
	}
}

func TestLocalizedOnlyOnTextFields(t *testing.T) {
	const bad = `
object: Thing
module: test
persistence: crud
fields:
  - {name: price_minor, type: money, localized: true}
permissions: {read: [t.viewer]}
`
	if _, err := ParseObject([]byte(bad)); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("ParseObject on a localized money field: err = %v, want ErrInvalidObject", err)
	}
}

// The translations key and its storage namespace both collide with field names
// in the custom_fields blob, so neither may be a field.
func TestReservedFieldNamesRejected(t *testing.T) {
	for _, name := range []string{TranslationsKey, translationsBlobKey} {
		t.Run(name, func(t *testing.T) {
			yaml := `
object: Thing
module: test
persistence: crud
fields:
  - {name: ` + name + `, type: text}
permissions: {read: [t.viewer]}
`
			if _, err := ParseObject([]byte(yaml)); !errors.Is(err, ErrInvalidObject) {
				t.Errorf("ParseObject with field %q: err = %v, want ErrInvalidObject", name, err)
			}

			core, err := ParseObject([]byte(localizedItemYAML))
			if err != nil {
				t.Fatalf("ParseObject: %v", err)
			}
			overlay := Overlay{Layer: "tenant", AddFields: []Field{{Name: name, Type: FieldText}}}
			if _, err := Merge(core, overlay); !errors.Is(err, ErrInvalidObject) {
				t.Errorf("Merge with overlay field %q: err = %v, want ErrInvalidObject", name, err)
			}
		})
	}
}

// Marking a field localized changes no column: it must not produce DDL, or
// every translated field would become a migration.
func TestLocalizingAFieldIsNotADDLChange(t *testing.T) {
	before, err := Merge(mustParse(t, localizedItemYAML))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	after, err := Merge(mustParse(t, localizedItemYAML))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for i := range after.Fields {
		if after.Fields[i].Name == "sku" {
			after.Fields[i].Localized = true
		}
	}

	plan, err := PlanEvolution(before, after, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("localizing a field planned %d DDL steps, want none: %+v", len(plan.Steps), plan.Steps)
	}
}

func mustParse(t *testing.T, yaml string) *Object {
	t.Helper()
	obj, err := ParseObject([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	return obj
}

func assertTranslation(t *testing.T, rec Record, field, locale, want string) {
	t.Helper()
	translations, ok := rec[TranslationsKey].(Translations)
	if !ok {
		t.Fatalf("record has no %s (%T): %v", TranslationsKey, rec[TranslationsKey], rec[TranslationsKey])
	}
	value, ok := translations[field]
	if !ok {
		t.Fatalf("no translations for field %q", field)
	}
	if got := value[locale]; got != want {
		t.Errorf("translations[%s][%s] = %q, want %q", field, locale, got, want)
	}
	// And the typed resolver agrees with the raw map.
	if got := i18n.Localized(value).For(mustTag(t, locale)); got != want {
		t.Errorf("Localized.For(%s) = %q, want %q", locale, got, want)
	}
}

func mustTag(t *testing.T, locale string) language.Tag {
	t.Helper()
	tag, err := language.Parse(locale)
	if err != nil {
		t.Fatalf("parse locale %q: %v", locale, err)
	}
	return tag
}

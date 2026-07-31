// SPDX-License-Identifier: AGPL-3.0-only

// Package contacts is the shared Contacts module (docs/10): a customer/vendor
// directory other modules reference by id. It provides the `contacts`
// capability that invoicing, payables, CRM, inventory and HR all require. A
// Contact is a plain CRUD object; modules that need one store its id (a link),
// they do not import this package's write path. Modules import kernel/* only
// (and, per the WP-1.4 boundary decision, a sibling module they declare in
// `requires:` — contacts is referenced by id, so no importer imports it).
package contacts

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ObjectContact is the metadata object name owned by this module (mirrored in
// contacts.yaml's objects: list).
const ObjectContact = "Contact"

// Contact kinds (closed set). A contact can be a customer, a vendor, or both.
const (
	KindCustomer = "customer"
	KindVendor   = "vendor"
	KindBoth     = "both"
)

var validKinds = map[string]bool{KindCustomer: true, KindVendor: true, KindBoth: true}

// ErrInvalidKind is returned by CreateContact for a kind outside the closed set.
var ErrInvalidKind = errors.New("contacts: invalid contact kind")

// locale is the counterparty's language — a BCP-47 tag ("de", "de-AT"), blank
// when unknown. Documents addressed to a contact render in it (docs/17: "invoice
// to a French customer prints in French from the same record"); it is copied
// onto a document when the draft is created, never read back at render time,
// because a posted document's language must not change under it (INV-F2).
const contactYAML = `
object: Contact
module: contacts
persistence: crud
fields:
  - {name: name, type: text, required: true, index: true}
  - {name: email, type: email}
  - {name: kind, type: enum, required: true}
  - {name: locale, type: text}
permissions:
  read: [contacts.viewer]
  create: [contacts.admin]
  update: [contacts.admin]
  delete: [contacts.admin]
`

// contactVersion is 2 because WP-1.7 added the optional locale field: an
// additive column, planned by metadata's evolution path on an existing
// database and created inline on a fresh one.
const contactVersion = 2

func effective(yaml string) (*metadata.EffectiveSchema, error) {
	obj, err := metadata.ParseObject([]byte(yaml))
	if err != nil {
		return nil, err
	}
	return metadata.Merge(obj)
}

// ContactSchema is the Contact object's effective schema, for the gateway to
// expose as a generic CRUD resource. The composition root (internal/app)
// passes it to api.Config.Objects.
func ContactSchema() (*metadata.EffectiveSchema, error) { return effective(contactYAML) }

func contactCRUD() (*metadata.CRUD, error) {
	eff, err := effective(contactYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewCRUD(eff)
}

// Register persists the Contact schema (core layer) and applies its DDL.
func Register(ctx context.Context, db *storage.DB) error {
	eff, err := effective(contactYAML)
	if err != nil {
		return err
	}
	if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, ObjectContact, contactVersion, []byte(contactYAML)); err != nil {
		return err
	}
	return metadata.ApplyDDL(ctx, db, eff, contactVersion)
}

// LocaleOf returns a contact's preferred language, or "" when it has none.
// Reading it is an ordinary authorized, tenant-scoped Contact read (INV-T1/T2)
// — there is no privileged path just because the caller only wants one field.
func LocaleOf(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (string, error) {
	crud, err := contactCRUD()
	if err != nil {
		return "", err
	}
	rec, err := crud.Get(ctx, db, tenant, id)
	if err != nil {
		return "", err
	}
	locale, _ := rec["locale"].(string)
	return locale, nil
}

// CreateContact adds a contact. kind must be one of the closed set.
func CreateContact(ctx context.Context, db *storage.DB, tenant tenancy.ID, name, email, kind string) (metadata.Record, error) {
	if !validKinds[kind] {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}
	crud, err := contactCRUD()
	if err != nil {
		return nil, err
	}
	rec := metadata.Record{"name": name, "kind": kind}
	if email != "" {
		rec["email"] = email
	}
	return crud.Create(ctx, db, tenant, rec)
}

// SetLocale records a contact's preferred language (a BCP-47 tag, or "" to
// clear it). It is an ordinary authorized Contact update — documents copy the
// value when they are drafted, so changing it never rewrites a posted document
// (INV-F2).
func SetLocale(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, locale string) error {
	crud, err := contactCRUD()
	if err != nil {
		return err
	}
	_, err = crud.Update(ctx, db, tenant, id, metadata.Record{"locale": locale})
	return err
}

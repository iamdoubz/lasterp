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

const contactYAML = `
object: Contact
module: contacts
persistence: crud
fields:
  - {name: name, type: text, required: true, index: true}
  - {name: email, type: email}
  - {name: kind, type: enum, required: true}
permissions:
  read: [contacts.viewer]
  create: [contacts.admin]
  update: [contacts.admin]
  delete: [contacts.admin]
`

func effective(yaml string) (*metadata.EffectiveSchema, error) {
	obj, err := metadata.ParseObject([]byte(yaml))
	if err != nil {
		return nil, err
	}
	return metadata.Merge(obj)
}

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
	if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, ObjectContact, 1, []byte(contactYAML)); err != nil {
		return err
	}
	return metadata.ApplyDDL(ctx, db, eff, 1)
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

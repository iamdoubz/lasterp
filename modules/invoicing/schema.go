// SPDX-License-Identifier: AGPL-3.0-only

// Package invoicing is the M2 Invoicing/AR module (docs/10), minus payments and
// dunning (WP-1.4). A customer invoice starts as an editable draft, then posts:
// tax is resolved (modules/tax), a gapless number is assigned at acceptance
// (INV-F6), and the amounts post to the general ledger through a declared
// template (INV-F5) as a balanced journal entry (INV-F1). A posted invoice is
// immutable (INV-F2) — enforced by a storage trigger on its table — and its
// financial effect lives in the append-only ledger.
//
// Per the WP-1.4 boundary decision (docs/notes/WP-1.4-decisions.md §1), this
// module imports the sibling modules it declares in `requires:` —
// modules/ledger and modules/tax — for synchronous GL posting; contacts is
// referenced by id only (no import). No import cycle: ledger and tax import
// kernel/* only.
package invoicing

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
)

// ObjectInvoice is the metadata object name owned by this module (mirrored in
// invoicing.yaml's objects: list).
const ObjectInvoice = "Invoice"

// Invoice statuses. draft is editable; posted is immutable (INV-F2). void /
// credit notes are a later WP (see decisions §7).
const (
	StatusDraft  = "draft"
	StatusPosted = "posted"
)

// ErrNotDraft is returned when a mutation targets an invoice that is not a draft
// (INV-F2 — the module guard; the storage trigger is the backstop).
var ErrNotDraft = errors.New("invoicing: invoice is not a draft")

// numberSeries is the document_number_series key invoices allocate from.
const numberSeries = "invoice"

// Invoice is a CRUD object. Lines are stored as a JSON array in the `lines`
// field (metadata child tables are unsupported for CRUD DDL — WP-0.5 decision
// 3); the module (de)serializes them. Money totals are integer minor units.
// number is blank until the invoice is posted (INV-F6: assigned only at
// acceptance).
const invoiceYAML = `
object: Invoice
module: invoicing
persistence: crud
fields:
  - {name: contact_id, type: link, target: Contact, required: true}
  - {name: currency, type: currency, required: true}
  - {name: status, type: enum, required: true}
  - {name: number, type: text, index: true}
  - {name: issue_date, type: text, required: true}
  - {name: ar_account, type: text, required: true}
  - {name: tax_account, type: text, required: true}
  - {name: lines, type: json, required: true}
  - {name: net_minor, type: int}
  - {name: tax_minor, type: int}
  - {name: gross_minor, type: int}
  - {name: gl_entry_id, type: text}
  - {name: posted_at, type: text}
permissions:
  read: [invoicing.viewer]
  create: [invoicing.clerk]
  update: [invoicing.clerk]
  delete: [invoicing.clerk]
  post: [invoicing.poster]
`

func effective(yaml string) (*metadata.EffectiveSchema, error) {
	obj, err := metadata.ParseObject([]byte(yaml))
	if err != nil {
		return nil, err
	}
	return metadata.Merge(obj)
}

func invoiceCRUD() (*metadata.CRUD, error) {
	eff, err := effective(invoiceYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewCRUD(eff)
}

// Register persists the Invoice schema (core layer), applies its DDL, and
// installs the INV-F2 immutability trigger on the generated table. The trigger
// cannot be a numbered migration: the obj_invoice table only exists once
// ApplyDDL has run at runtime (same reason ledger's triggers live on
// migration-created tables only).
func Register(ctx context.Context, db *storage.DB) error {
	eff, err := effective(invoiceYAML)
	if err != nil {
		return err
	}
	if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, ObjectInvoice, 1, []byte(invoiceYAML)); err != nil {
		return err
	}
	if err := metadata.ApplyDDL(ctx, db, eff, 1); err != nil {
		return err
	}
	return enforceInvoiceImmutability(ctx, db)
}

// enforceInvoiceImmutability installs a BEFORE UPDATE/DELETE trigger on
// obj_invoice that rejects any mutation of a row whose *existing* status is
// already 'posted' (INV-F2, storage layer). The draft→posted transition itself
// passes because OLD.status is 'draft' at that point; every mutation after is
// refused. Idempotent (drops-if-exists first) so Register can run repeatedly.
func enforceInvoiceImmutability(ctx context.Context, db *storage.DB) error {
	table := metadata.TableName(ObjectInvoice)
	var stmts []string
	if db.Dialect == storage.Postgres {
		stmts = []string{
			`DROP TRIGGER IF EXISTS invoice_posted_no_update ON ` + table,
			`DROP TRIGGER IF EXISTS invoice_posted_no_delete ON ` + table,
			`CREATE OR REPLACE FUNCTION reject_posted_invoice_mutation() RETURNS TRIGGER AS $$
			 BEGIN
				RAISE EXCEPTION 'invoicing: posted invoice is immutable: % not permitted', TG_OP;
			 END;
			 $$ LANGUAGE plpgsql`,
			`CREATE TRIGGER invoice_posted_no_update BEFORE UPDATE ON ` + table + `
			 FOR EACH ROW WHEN (OLD.status = 'posted') EXECUTE FUNCTION reject_posted_invoice_mutation()`,
			`CREATE TRIGGER invoice_posted_no_delete BEFORE DELETE ON ` + table + `
			 FOR EACH ROW WHEN (OLD.status = 'posted') EXECUTE FUNCTION reject_posted_invoice_mutation()`,
		}
	} else {
		stmts = []string{
			`DROP TRIGGER IF EXISTS invoice_posted_no_update`,
			`DROP TRIGGER IF EXISTS invoice_posted_no_delete`,
			`CREATE TRIGGER invoice_posted_no_update BEFORE UPDATE ON ` + table + `
			 WHEN OLD.status = 'posted'
			 BEGIN SELECT RAISE(ABORT, 'invoicing: posted invoice is immutable: UPDATE not permitted'); END`,
			`CREATE TRIGGER invoice_posted_no_delete BEFORE DELETE ON ` + table + `
			 WHEN OLD.status = 'posted'
			 BEGIN SELECT RAISE(ABORT, 'invoicing: posted invoice is immutable: DELETE not permitted'); END`,
		}
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("invoicing: install immutability trigger: %w", err)
		}
	}
	return nil
}

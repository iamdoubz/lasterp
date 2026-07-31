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
	"strings"

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
//
// locale is the language the document renders in — the counterparty's, copied
// from the contact when the draft is created (WP-1.7). It belongs on the
// document rather than being looked up at render time because a posted invoice
// is immutable (INV-F2): what it said when it was sent is what it must keep
// saying, whatever happens to the contact record afterwards.
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
  - {name: locale, type: text}
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

// Register persists this module's schemas (core layer), applies their DDL, and
// installs the INV-F2 immutability triggers on the generated tables. The
// triggers cannot be numbered migrations: obj_invoice / obj_receipt only exist
// once ApplyDDL has run at runtime (same reason ledger's triggers live on
// migration-created tables only).
func Register(ctx context.Context, db *storage.DB) error {
	for _, o := range []struct {
		name    string
		yaml    string
		version int
	}{
		// Invoice is at v2: WP-1.7 added the optional locale column, which the
		// metadata engine adds by ALTER on a database that already holds v1.
		{ObjectInvoice, invoiceYAML, 2},
		{ObjectReceipt, receiptYAML, 1},
	} {
		eff, err := effective(o.yaml)
		if err != nil {
			return err
		}
		if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, o.name, o.version, []byte(o.yaml)); err != nil {
			return err
		}
		if err := metadata.ApplyDDL(ctx, db, eff, o.version); err != nil {
			return err
		}
		// Both are posting documents, so both are frozen once posted (INV-F2).
		if err := enforcePostedImmutability(ctx, db, o.name); err != nil {
			return err
		}
	}
	return nil
}

// enforcePostedImmutability installs BEFORE UPDATE/DELETE triggers on a posting
// document's generated table that reject any mutation of a row whose *existing*
// status is already 'posted' (INV-F2, storage layer). The draft→posted
// transition itself passes because OLD.status is 'draft' at that point; every
// mutation after is refused. Idempotent (drops-if-exists first) so Register can
// run repeatedly — which it does on every boot.
//
// object is a kernel-controlled metadata object name (invoicing's own
// constants), never caller input, so interpolating it into the trigger name and
// table is not an injection surface.
func enforcePostedImmutability(ctx context.Context, db *storage.DB, object string) error {
	table := metadata.TableName(object)
	lower := strings.ToLower(object)
	fn := "reject_posted_" + lower + "_mutation"
	msg := "invoicing: posted " + lower + " is immutable"

	var stmts []string
	if db.Dialect == storage.Postgres {
		stmts = []string{
			`DROP TRIGGER IF EXISTS ` + lower + `_posted_no_update ON ` + table,
			`DROP TRIGGER IF EXISTS ` + lower + `_posted_no_delete ON ` + table,
			`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS TRIGGER AS $$
			 BEGIN
				RAISE EXCEPTION '` + msg + `: % not permitted', TG_OP;
			 END;
			 $$ LANGUAGE plpgsql`,
			`CREATE TRIGGER ` + lower + `_posted_no_update BEFORE UPDATE ON ` + table + `
			 FOR EACH ROW WHEN (OLD.status = 'posted') EXECUTE FUNCTION ` + fn + `()`,
			`CREATE TRIGGER ` + lower + `_posted_no_delete BEFORE DELETE ON ` + table + `
			 FOR EACH ROW WHEN (OLD.status = 'posted') EXECUTE FUNCTION ` + fn + `()`,
		}
	} else {
		stmts = []string{
			`DROP TRIGGER IF EXISTS ` + lower + `_posted_no_update`,
			`DROP TRIGGER IF EXISTS ` + lower + `_posted_no_delete`,
			`CREATE TRIGGER ` + lower + `_posted_no_update BEFORE UPDATE ON ` + table + `
			 WHEN OLD.status = 'posted'
			 BEGIN SELECT RAISE(ABORT, '` + msg + `: UPDATE not permitted'); END`,
			`CREATE TRIGGER ` + lower + `_posted_no_delete BEFORE DELETE ON ` + table + `
			 WHEN OLD.status = 'posted'
			 BEGIN SELECT RAISE(ABORT, '` + msg + `: DELETE not permitted'); END`,
		}
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("invoicing: install %s immutability trigger: %w", lower, err)
		}
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ObjectReceipt is the metadata object name for a customer receipt (AR payment).
const ObjectReceipt = "Receipt"

// receiptSeries is the document_number_series key receipts allocate from. It is
// separate from the invoice series: two document types sharing one counter would
// make both look gappy (INV-F6 is per-series).
const receiptSeries = "receipt"

// ErrReceiptNotFound is returned by GetReceipt for an unknown id.
var ErrReceiptNotFound = errors.New("invoicing: receipt not found")

// ErrOverApplied is the INV-F8 refusal: a receipt would settle more than the
// invoice's gross, counting every other posted receipt already applied to it.
var ErrOverApplied = errors.New("invoicing: receipt over-applies an invoice")

// Application is one invoice a receipt settles, and by how much. Splitting a
// payment across invoices is the normal case in AR (one cheque, five invoices),
// so this is a list from the start rather than a single invoice_id.
//
// A receipt's amount *is* the sum of its applications — there is deliberately no
// separate "amount received" that could differ. Unapplied cash on account is a
// distinct concept (a customer credit balance) belonging to the Phase-4 payment
// work; allowing total ≠ Σapplications now would create an unreconciled
// difference with nowhere to show it and nothing to reconcile it against.
type Application struct {
	InvoiceID   string `json:"invoice_id"`
	AmountMinor int64  `json:"amount_minor"`
}

// ReceiptInput is the data to create or replace a receipt draft.
type ReceiptInput struct {
	ContactID    string // who paid
	Currency     string // ISO-4217; must match every invoice it applies to
	ReceivedDate string // YYYY-MM-DD
	BankAccount  string // GL account the money landed in (debit side)
	Applications []Application
}

// Receipt is a draft or posted customer receipt.
type Receipt struct {
	ID           string
	ContactID    string
	Currency     string
	Status       string
	Number       string
	ReceivedDate string
	BankAccount  string
	Applications []Application
	TotalMinor   int64
	GLEntryID    string
	PostedAt     string
}

// receiptYAML is the Receipt object. Applications are stored as a JSON array for
// the same reason invoice lines are (no CRUD child tables — WP-0.5 decision 3).
// number is blank until the receipt is posted (INV-F6).
const receiptYAML = `
object: Receipt
module: invoicing
persistence: crud
fields:
  - {name: contact_id, type: link, target: Contact, required: true}
  - {name: currency, type: currency, required: true}
  - {name: status, type: enum, required: true, options: [draft, posted]}
  - {name: number, type: text, index: true}
  - {name: received_date, type: text, required: true}
  - {name: bank_account, type: text, required: true}
  - {name: applications, type: json, required: true}
  - {name: total_minor, type: int}
  - {name: gl_entry_id, type: text}
  - {name: posted_at, type: text}
permissions:
  read: [invoicing.viewer]
  create: [invoicing.clerk]
  update: [invoicing.clerk]
  delete: [invoicing.clerk]
  post: [invoicing.poster]
`

func receiptCRUD() (*metadata.CRUD, error) {
	eff, err := effective(receiptYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewCRUD(eff)
}

// ReceiptSchema is the effective Receipt schema, for the composition root's
// metadata surface.
func ReceiptSchema() (*metadata.EffectiveSchema, error) { return effective(receiptYAML) }

// validateReceipt checks the structural rules a draft must satisfy. Whether the
// applications fit within the invoices' outstanding amounts (INV-F8) is checked
// at post time, under the row lock — an outstanding balance read now could be
// stale by the time the receipt posts.
func validateReceipt(in ReceiptInput) error {
	if in.ContactID == "" {
		return errors.New("invoicing: contact_id is required")
	}
	if _, err := money.Lookup(in.Currency); err != nil {
		return err
	}
	if in.ReceivedDate == "" {
		return errors.New("invoicing: received_date is required")
	}
	if in.BankAccount == "" {
		return errors.New("invoicing: bank_account is required")
	}
	if len(in.Applications) == 0 {
		return errors.New("invoicing: a receipt must apply to at least one invoice")
	}
	seen := make(map[string]bool, len(in.Applications))
	for i, a := range in.Applications {
		if a.InvoiceID == "" {
			return fmt.Errorf("invoicing: application %d: invoice_id is required", i)
		}
		// Two applications to one invoice on one receipt would each pass an
		// individual bounds check while together breaching it (INV-F8).
		if seen[a.InvoiceID] {
			return fmt.Errorf("invoicing: application %d: invoice %q applied twice on one receipt", i, a.InvoiceID)
		}
		seen[a.InvoiceID] = true
		if a.AmountMinor <= 0 {
			return fmt.Errorf("invoicing: application %d: amount must be positive", i)
		}
	}
	return nil
}

// CreateReceiptDraft stores a new draft receipt (status=draft, no number, no GL
// effect).
func CreateReceiptDraft(ctx context.Context, db *storage.DB, tenant tenancy.ID, in ReceiptInput) (Receipt, error) {
	if err := validateReceipt(in); err != nil {
		return Receipt{}, err
	}
	crud, err := receiptCRUD()
	if err != nil {
		return Receipt{}, err
	}
	appsJSON, err := json.Marshal(in.Applications)
	if err != nil {
		return Receipt{}, err
	}
	out, err := crud.Create(ctx, db, tenant, metadata.Record{
		"contact_id":    in.ContactID,
		"currency":      in.Currency,
		"status":        StatusDraft,
		"number":        "",
		"received_date": in.ReceivedDate,
		"bank_account":  in.BankAccount,
		"applications":  string(appsJSON),
	})
	if err != nil {
		return Receipt{}, err
	}
	return recordToReceipt(out)
}

// UpdateReceiptDraft replaces a draft's editable content, refusing a posted
// receipt at the module layer (the storage trigger is the backstop, INV-F2).
func UpdateReceiptDraft(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, in ReceiptInput) (Receipt, error) {
	if err := validateReceipt(in); err != nil {
		return Receipt{}, err
	}
	current, err := GetReceipt(ctx, db, tenant, id)
	if err != nil {
		return Receipt{}, err
	}
	if current.Status != StatusDraft {
		return Receipt{}, fmt.Errorf("%w: %q", ErrNotDraft, id)
	}
	crud, err := receiptCRUD()
	if err != nil {
		return Receipt{}, err
	}
	appsJSON, err := json.Marshal(in.Applications)
	if err != nil {
		return Receipt{}, err
	}
	if _, err := crud.Update(ctx, db, tenant, id, metadata.Record{
		"contact_id":    in.ContactID,
		"currency":      in.Currency,
		"received_date": in.ReceivedDate,
		"bank_account":  in.BankAccount,
		"applications":  string(appsJSON),
	}); err != nil {
		return Receipt{}, err
	}
	return GetReceipt(ctx, db, tenant, id)
}

// GetReceipt reads a receipt back (authorized as Receipt "read").
func GetReceipt(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (Receipt, error) {
	crud, err := receiptCRUD()
	if err != nil {
		return Receipt{}, err
	}
	rec, err := crud.Get(ctx, db, tenant, id)
	if errors.Is(err, metadata.ErrRecordNotFound) {
		return Receipt{}, fmt.Errorf("%w: %q", ErrReceiptNotFound, id)
	}
	if err != nil {
		return Receipt{}, err
	}
	return recordToReceipt(rec)
}

func recordToReceipt(rec metadata.Record) (Receipt, error) {
	r := Receipt{
		ID:           asString(rec["id"]),
		ContactID:    asString(rec["contact_id"]),
		Currency:     asString(rec["currency"]),
		Status:       asString(rec["status"]),
		Number:       asString(rec["number"]),
		ReceivedDate: asString(rec["received_date"]),
		BankAccount:  asString(rec["bank_account"]),
		TotalMinor:   asInt64(rec["total_minor"]),
		GLEntryID:    asString(rec["gl_entry_id"]),
		PostedAt:     asString(rec["posted_at"]),
	}
	if s := asString(rec["applications"]); s != "" {
		if err := json.Unmarshal([]byte(s), &r.Applications); err != nil {
			return Receipt{}, fmt.Errorf("invoicing: decode applications for %q: %w", r.ID, err)
		}
	}
	return r, nil
}

// formatReceiptNumber renders an allocated sequence value as a receipt number.
// ponytail: fixed "RCT-000001" width, mirroring formatInvoiceNumber; per-tenant
// numbering policy is one customization field to add when a tenant asks.
func formatReceiptNumber(seq int64) string {
	return fmt.Sprintf("RCT-%06d", seq)
}

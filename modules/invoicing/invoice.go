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

// Line is one invoice line. Amounts are in the invoice currency's minor units
// (never float — INV-F4). Net = UnitPriceMinor × Quantity; tax is resolved at
// post time from (TaxJurisdiction, TaxCategory). RevenueAccount is the GL
// account the line's net credits (INV-F5 template).
type Line struct {
	Description     string `json:"description"`
	Quantity        int64  `json:"quantity"`
	UnitPriceMinor  int64  `json:"unit_price_minor"`
	RevenueAccount  string `json:"revenue_account"`
	TaxJurisdiction string `json:"tax_jurisdiction"`
	TaxCategory     string `json:"tax_category"`
}

// netMinor is the line's pre-tax amount in minor units.
func (l Line) netMinor() int64 { return l.UnitPriceMinor * l.Quantity }

// DraftInput is the data to create or replace an invoice draft.
type DraftInput struct {
	ContactID  string
	Currency   string // ISO-4217; all lines share it (v1 single-currency invoice)
	IssueDate  string // YYYY-MM-DD
	ARAccount  string // GL accounts-receivable control account (debit side)
	TaxAccount string // GL tax-payable account (credit side)
	Lines      []Line
}

// Invoice is a draft or posted invoice, decoded from its CRUD record.
type Invoice struct {
	ID         string
	ContactID  string
	Currency   string
	Status     string
	Number     string
	IssueDate  string
	ARAccount  string
	TaxAccount string
	Lines      []Line
	NetMinor   int64
	TaxMinor   int64
	GrossMinor int64
	GLEntryID  string
	PostedAt   string
}

// ErrInvoiceNotFound is returned by GetInvoice for an unknown id.
var ErrInvoiceNotFound = errors.New("invoicing: invoice not found")

// validateDraft checks the structural rules a draft must satisfy before it can
// be stored (currency valid, at least one line, accounts present, non-negative
// amounts). Tax correctness is checked at post time against the rate store.
func validateDraft(in DraftInput) error {
	if in.ContactID == "" {
		return errors.New("invoicing: contact_id is required")
	}
	if _, err := money.Lookup(in.Currency); err != nil {
		return err
	}
	if in.IssueDate == "" {
		return errors.New("invoicing: issue_date is required")
	}
	if in.ARAccount == "" || in.TaxAccount == "" {
		return errors.New("invoicing: ar_account and tax_account are required")
	}
	if len(in.Lines) == 0 {
		return errors.New("invoicing: an invoice needs at least one line")
	}
	for i, l := range in.Lines {
		if l.RevenueAccount == "" {
			return fmt.Errorf("invoicing: line %d: revenue_account is required", i)
		}
		if l.Quantity < 0 || l.UnitPriceMinor < 0 {
			return fmt.Errorf("invoicing: line %d: quantity and unit price must be non-negative", i)
		}
		if l.TaxJurisdiction == "" || l.TaxCategory == "" {
			return fmt.Errorf("invoicing: line %d: tax_jurisdiction and tax_category are required", i)
		}
	}
	return nil
}

// CreateDraft stores a new draft invoice (status=draft, no number, no GL
// effect). It is authorized as an Invoice "create" (INV-T2/T4 via the CRUD
// engine).
func CreateDraft(ctx context.Context, db *storage.DB, tenant tenancy.ID, in DraftInput) (Invoice, error) {
	if err := validateDraft(in); err != nil {
		return Invoice{}, err
	}
	crud, err := invoiceCRUD()
	if err != nil {
		return Invoice{}, err
	}
	linesJSON, err := json.Marshal(in.Lines)
	if err != nil {
		return Invoice{}, err
	}
	rec := metadata.Record{
		"contact_id":  in.ContactID,
		"currency":    in.Currency,
		"status":      StatusDraft,
		"number":      "",
		"issue_date":  in.IssueDate,
		"ar_account":  in.ARAccount,
		"tax_account": in.TaxAccount,
		"lines":       string(linesJSON),
	}
	out, err := crud.Create(ctx, db, tenant, rec)
	if err != nil {
		return Invoice{}, err
	}
	return recordToInvoice(out)
}

// UpdateDraft replaces a draft's editable content. It refuses a non-draft
// invoice at the module layer (ErrNotDraft); the storage trigger is the
// backstop if a generic update path is ever attempted on a posted row (INV-F2).
func UpdateDraft(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, in DraftInput) (Invoice, error) {
	if err := validateDraft(in); err != nil {
		return Invoice{}, err
	}
	current, err := GetInvoice(ctx, db, tenant, id)
	if err != nil {
		return Invoice{}, err
	}
	if current.Status != StatusDraft {
		return Invoice{}, fmt.Errorf("%w: %q", ErrNotDraft, id)
	}
	crud, err := invoiceCRUD()
	if err != nil {
		return Invoice{}, err
	}
	linesJSON, err := json.Marshal(in.Lines)
	if err != nil {
		return Invoice{}, err
	}
	_, err = crud.Update(ctx, db, tenant, id, metadata.Record{
		"contact_id":  in.ContactID,
		"currency":    in.Currency,
		"issue_date":  in.IssueDate,
		"ar_account":  in.ARAccount,
		"tax_account": in.TaxAccount,
		"lines":       string(linesJSON),
	})
	if err != nil {
		return Invoice{}, err
	}
	return GetInvoice(ctx, db, tenant, id)
}

// GetInvoice reads an invoice back (authorized as Invoice "read").
func GetInvoice(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (Invoice, error) {
	crud, err := invoiceCRUD()
	if err != nil {
		return Invoice{}, err
	}
	rec, err := crud.Get(ctx, db, tenant, id)
	if errors.Is(err, metadata.ErrRecordNotFound) {
		return Invoice{}, fmt.Errorf("%w: %q", ErrInvoiceNotFound, id)
	}
	if err != nil {
		return Invoice{}, err
	}
	return recordToInvoice(rec)
}

// recordToInvoice decodes a CRUD Record into an Invoice. Int fields come back as
// int64 (or nil when unset); the lines JSON is parsed.
func recordToInvoice(rec metadata.Record) (Invoice, error) {
	inv := Invoice{
		ID:         asString(rec["id"]),
		ContactID:  asString(rec["contact_id"]),
		Currency:   asString(rec["currency"]),
		Status:     asString(rec["status"]),
		Number:     asString(rec["number"]),
		IssueDate:  asString(rec["issue_date"]),
		ARAccount:  asString(rec["ar_account"]),
		TaxAccount: asString(rec["tax_account"]),
		NetMinor:   asInt64(rec["net_minor"]),
		TaxMinor:   asInt64(rec["tax_minor"]),
		GrossMinor: asInt64(rec["gross_minor"]),
		GLEntryID:  asString(rec["gl_entry_id"]),
		PostedAt:   asString(rec["posted_at"]),
	}
	if s := asString(rec["lines"]); s != "" {
		if err := json.Unmarshal([]byte(s), &inv.Lines); err != nil {
			return Invoice{}, fmt.Errorf("invoicing: decode lines for %q: %w", inv.ID, err)
		}
	}
	return inv, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

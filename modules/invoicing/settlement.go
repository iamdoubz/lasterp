// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Settlement states. These describe how much of a posted invoice has been paid.
// They are DERIVED from posted receipts and are never written to the invoice row
// — a posted invoice is immutable (INV-F2), so "paid" is a computed view of the
// document, not an edit to it (WP-1.6-decisions.md §1). Invoice.Status keeps its
// document meaning (draft / posted); this is a separate axis.
const (
	SettlementOpen    = "open"
	SettlementPartial = "partial"
	SettlementPaid    = "paid"
)

// Settlement is an invoice's derived payment position, in the invoice currency's
// minor units.
type Settlement struct {
	InvoiceID        string
	GrossMinor       int64
	SettledMinor     int64
	OutstandingMinor int64
	Status           string
}

// settlementStatus classifies an outstanding balance. Nothing settled is open;
// fully settled is paid; anything between is partial. Over-application cannot
// occur (INV-F8), so outstanding is never negative — but if a future path ever
// let it, this reports paid rather than inventing a fourth state, and the
// invariant test is what fails.
func settlementStatus(gross, settled int64) string {
	switch {
	case settled <= 0:
		return SettlementOpen
	case settled >= gross:
		return SettlementPaid
	default:
		return SettlementPartial
	}
}

// SettlementFor computes one invoice's settlement position from the posted
// receipts applied to it. A draft invoice has no settlement position (nothing
// can be applied to an unposted document), and reports as open with its gross
// outstanding.
func SettlementFor(ctx context.Context, db *storage.DB, tenant tenancy.ID, inv Invoice) (Settlement, error) {
	var settled int64
	if inv.Status == StatusPosted {
		var err error
		settled, err = AppliedToInvoice(ctx, db, tenant, inv.ID)
		if err != nil {
			return Settlement{}, err
		}
	}
	return Settlement{
		InvoiceID:        inv.ID,
		GrossMinor:       inv.GrossMinor,
		SettledMinor:     settled,
		OutstandingMinor: inv.GrossMinor - settled,
		Status:           settlementStatus(inv.GrossMinor, settled),
	}, nil
}

// AppliedToInvoice sums every posted receipt application against invoiceID.
// Draft receipts are excluded by design: an unposted receipt has no GL effect,
// so counting it would show an invoice as paid while the ledger still carries
// the receivable.
func AppliedToInvoice(ctx context.Context, db *storage.DB, tenant tenancy.ID, invoiceID string) (int64, error) {
	var total int64
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		total, err = appliedToInvoiceTx(ctx, tx, db, tenant, invoiceID, "")
		return err
	})
	return total, err
}

// appliedToInvoiceTx is AppliedToInvoice inside an existing transaction, so the
// INV-F8 check can read it under the same lock that commits the acceptance.
// excludeReceiptID skips one receipt (the one being posted), which keeps the
// check correct on a retry: the receipt's own row is still `draft` at that
// point, but excluding it explicitly means the check does not depend on that.
//
// ponytail: this scans the tenant's posted receipts and sums in Go rather than
// pushing a JSON aggregate into SQL — applications are a JSON array, and the two
// dialects disagree about JSON functions (ADR-015 portability). Swap for a
// receipt_applications side table if AR volume ever makes the scan hurt; the
// signature does not change.
func appliedToInvoiceTx(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, invoiceID, excludeReceiptID string) (int64, error) {
	table := metadata.TableName(ObjectReceipt)
	rows, err := tx.QueryContext(ctx, db.Rebind(
		`SELECT id, applications FROM `+table+`
		 WHERE tenant_id = ? AND status = ? AND archived_at IS NULL`),
		string(tenant), StatusPosted)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var total int64
	for rows.Next() {
		var id, appsJSON string
		if err := rows.Scan(&id, &appsJSON); err != nil {
			return 0, err
		}
		if id == excludeReceiptID || appsJSON == "" {
			continue
		}
		var apps []Application
		if err := json.Unmarshal([]byte(appsJSON), &apps); err != nil {
			return 0, err
		}
		for _, a := range apps {
			if a.InvoiceID == invoiceID {
				total += a.AmountMinor
			}
		}
	}
	return total, rows.Err()
}

// idgenNew is a thin alias so receipt_post.go's audit insert reads like
// invoice post's, without a second idgen import there.
func idgenNew() string { return idgen.New() }

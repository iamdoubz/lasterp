//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-1.6 PR-A over HTTP: the AR receipt lifecycle is reachable through the
// product API, and the invariants hold at the edge as well as in the module.

// TestReceiptLifecycleOverHTTP drives invoice → receipt → settlement entirely
// over the wire, which is what "every capability is reachable via API" means.
func TestReceiptLifecycleOverHTTP(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			ar := e.createAccount("1100", "Accounts receivable", "asset")
			rev := e.createAccount("4000", "Revenue", "income")
			taxAcct := e.createAccount("2200", "Tax payable", "liability")
			bank := e.createAccount("1000", "Bank", "asset")

			status, body, contact := e.post("/api/v1/contact", map[string]any{
				"name": "Wile E. Coyote", "email": idgen.New() + "@acme.test", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create contact = %d; body=%s", status, body)
			}
			contactID := mustField(t, contact, "id")

			if status, body, _ := e.post("/api/v1/periods", map[string]any{
				"code": "2026-07", "start_date": "2026-07-01", "end_date": "2026-07-31",
			}); status != http.StatusCreated {
				t.Fatalf("create period = %d; body=%s", status, body)
			}
			if status, body, _ := e.post("/api/v1/taxrates", map[string]any{
				"jurisdiction": "DE", "category": "standard", "rate": "0.20",
				"as_of": "2020-01-01", "name": "USt", "provider": "test",
			}); status != http.StatusCreated {
				t.Fatalf("save tax rate = %d; body=%s", status, body)
			}

			// Invoice: 1000.00 net + 20% = 1200.00 gross.
			status, body, draft := e.post("/api/v1/invoices", map[string]any{
				"contact_id": contactID, "currency": "EUR", "issue_date": "2026-07-15",
				"ar_account": ar, "tax_account": taxAcct,
				"lines": []map[string]any{{
					"description": "Rocket skates", "quantity": 1, "unit_price_minor": 100000,
					"revenue_account": rev, "tax_jurisdiction": "DE", "tax_category": "standard",
				}},
			})
			if status != http.StatusCreated {
				t.Fatalf("create invoice = %d; body=%s", status, body)
			}
			invoiceID := mustField(t, draft, "ID")

			status, body, posted := e.post("/api/v1/invoices/"+invoiceID+"/post", map[string]any{"period": "2026-07"})
			if status != http.StatusOK {
				t.Fatalf("post invoice = %d; body=%s", status, body)
			}
			gross, ok := posted["GrossMinor"].(float64)
			if !ok || gross != 120000 {
				t.Fatalf("invoice gross = %v, want 120000", posted["GrossMinor"])
			}

			// Nothing paid yet.
			assertSettlement(t, e, invoiceID, "open", 120000)

			// Part payment over HTTP.
			status, body, receipt := e.post("/api/v1/receipts", map[string]any{
				"contact_id": contactID, "currency": "EUR", "received_date": "2026-07-20",
				"bank_account": bank,
				"applications": []map[string]any{{"invoice_id": invoiceID, "amount_minor": 50000}},
			})
			if status != http.StatusCreated {
				t.Fatalf("create receipt = %d; body=%s", status, body)
			}
			receiptID := mustField(t, receipt, "ID")

			// A draft receipt settles nothing: no GL effect, no settlement.
			assertSettlement(t, e, invoiceID, "open", 120000)

			status, body, postedReceipt := e.post("/api/v1/receipts/"+receiptID+"/post", map[string]any{"period": "2026-07"})
			if status != http.StatusOK {
				t.Fatalf("post receipt = %d; body=%s", status, body)
			}
			if num := mustField(t, postedReceipt, "Number"); num != "RCT-000001" {
				t.Errorf("receipt number = %q, want RCT-000001 (allocated at acceptance)", num)
			}
			entryID := mustField(t, postedReceipt, "GLEntryID")

			// The receipt's GL entry balances (INV-F1).
			status, body, entry := e.get("/api/v1/journalentries/" + entryID)
			if status != http.StatusOK {
				t.Fatalf("get journal entry = %d; body=%s", status, body)
			}
			assertBalanced(t, entry, 50000)

			assertSettlement(t, e, invoiceID, "partial", 70000)

			// Settle the rest.
			_, _, rest := e.post("/api/v1/receipts", map[string]any{
				"contact_id": contactID, "currency": "EUR", "received_date": "2026-07-21",
				"bank_account": bank,
				"applications": []map[string]any{{"invoice_id": invoiceID, "amount_minor": 70000}},
			})
			restID := mustField(t, rest, "ID")
			if status, body, _ := e.post("/api/v1/receipts/"+restID+"/post", map[string]any{"period": "2026-07"}); status != http.StatusOK {
				t.Fatalf("post second receipt = %d; body=%s", status, body)
			}
			assertSettlement(t, e, invoiceID, "paid", 0)

			// The invoice document is untouched throughout (INV-F2).
			_, _, final := e.get("/api/v1/invoices/" + invoiceID)
			if got := mustField(t, final, "Status"); got != "posted" {
				t.Errorf("invoice status = %q, want it to stay posted — settlement is derived, not stamped", got)
			}

			// INV-F8 at the edge: one more cent is refused with a 4xx that names
			// the problem, not an opaque 500.
			_, _, extra := e.post("/api/v1/receipts", map[string]any{
				"contact_id": contactID, "currency": "EUR", "received_date": "2026-07-22",
				"bank_account": bank,
				"applications": []map[string]any{{"invoice_id": invoiceID, "amount_minor": 1}},
			})
			extraID := mustField(t, extra, "ID")
			status, body, _ = e.post("/api/v1/receipts/"+extraID+"/post", map[string]any{"period": "2026-07"})
			if status != http.StatusUnprocessableEntity {
				t.Errorf("over-application = %d, want 422; body=%s", status, body)
			}
			assertSettlement(t, e, invoiceID, "paid", 0)
		})
	}
}

// Receipt is a posting document, so like Invoice it must not have a generic
// CRUD route — a generic PATCH would let a client set status=posted with a
// hand-picked total, bypassing the pipeline (WP-1.4b-decisions.md §2).
func TestReceiptHasNoGenericCrudRoute(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if status, _, _ := e.get("/api/v1/receipt"); status != http.StatusNotFound {
				t.Errorf("generic Receipt collection route = %d, want 404 — financial documents are not generic CRUD", status)
			}
		})
	}
}

func assertSettlement(t *testing.T, e *env, invoiceID, wantStatus string, wantOutstanding float64) {
	t.Helper()
	status, body, s := e.get("/api/v1/invoices/" + invoiceID + "/settlement")
	if status != http.StatusOK {
		t.Fatalf("get settlement = %d; body=%s", status, body)
	}
	if got := mustField(t, s, "Status"); got != wantStatus {
		t.Errorf("settlement status = %q, want %q", got, wantStatus)
	}
	outstanding, ok := s["OutstandingMinor"].(float64)
	if !ok {
		outstanding = 0
	}
	if outstanding != wantOutstanding {
		t.Errorf("outstanding = %v, want %v", s["OutstandingMinor"], wantOutstanding)
	}
}

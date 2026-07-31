//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-1.6 PR-B over HTTP: reports, metrics and exports are reachable through the
// product API, and the permission gate holds at the edge as well as in the
// engine.

// booksOverHTTP posts an invoice and part-settles it, returning the invoice id
// and its gross.
func booksOverHTTP(t *testing.T, e *env) (string, float64) {
	t.Helper()
	ar := e.createAccount("1100", "Accounts receivable", "asset")
	rev := e.createAccount("4000", "Revenue", "income")
	taxAcct := e.createAccount("2200", "Tax payable", "liability")
	bank := e.createAccount("1000", "Bank", "asset")

	_, _, contact := e.post("/api/v1/contact", map[string]any{
		"name": "Acme", "email": idgen.New() + "@acme.test", "kind": "customer",
	})
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

	_, _, draft := e.post("/api/v1/invoices", map[string]any{
		"contact_id": contactID, "currency": "EUR", "issue_date": "2026-07-15",
		"ar_account": ar, "tax_account": taxAcct,
		"lines": []map[string]any{{
			"description": "Consulting", "quantity": 1, "unit_price_minor": 100000,
			"revenue_account": rev, "tax_jurisdiction": "DE", "tax_category": "standard",
		}},
	})
	invoiceID := mustField(t, draft, "ID")
	status, body, posted := e.post("/api/v1/invoices/"+invoiceID+"/post", map[string]any{"period": "2026-07"})
	if status != http.StatusOK {
		t.Fatalf("post invoice = %d; body=%s", status, body)
	}
	gross, _ := posted["GrossMinor"].(float64)

	// Settle half.
	_, _, receipt := e.post("/api/v1/receipts", map[string]any{
		"contact_id": contactID, "currency": "EUR", "received_date": "2026-07-20",
		"bank_account": bank,
		"applications": []map[string]any{{"invoice_id": invoiceID, "amount_minor": 50000}},
	})
	receiptID := mustField(t, receipt, "ID")
	if status, body, _ := e.post("/api/v1/receipts/"+receiptID+"/post", map[string]any{"period": "2026-07"}); status != http.StatusOK {
		t.Fatalf("post receipt = %d; body=%s", status, body)
	}
	return invoiceID, gross
}

// Every canned report runs over HTTP and reconciles with the books behind it.
func TestReportsOverHTTP(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, gross := booksOverHTTP(t, e)

			status, body, catalog := e.get("/api/v1/reports")
			if status != http.StatusOK {
				t.Fatalf("list reports = %d; body=%s", status, body)
			}
			if data, _ := catalog["data"].([]any); len(data) < 4 {
				t.Errorf("report catalog has %d entries, want the four canned reports", len(data))
			}

			for _, report := range []string{"trial_balance", "profit_and_loss", "balance_sheet", "ar_aging"} {
				status, body, rep := e.get("/api/v1/reports/" + report + "?currency=EUR&as_of=2026-07-31")
				if status != http.StatusOK {
					t.Fatalf("run %s = %d; body=%s", report, status, body)
				}
				if rows, _ := rep["rows"].([]any); len(rows) == 0 {
					t.Errorf("%s returned no rows against real books", report)
				}
			}

			// AR aging shows the unsettled half, proving the receipt reduced it.
			_, _, aging := e.get("/api/v1/reports/ar_aging?currency=EUR&as_of=2026-07-31")
			totals, _ := aging["totals"].([]any)
			var agingTotal float64
			for _, raw := range totals {
				row, _ := raw.(map[string]any)
				if row["key"] == "total" {
					agingTotal, _ = row["amount_minor"].(float64)
				}
			}
			if want := gross - 50000; agingTotal != want {
				t.Errorf("AR aging total = %v, want %v (gross less the receipt)", agingTotal, want)
			}
		})
	}
}

// A missing currency is a 400 with a usable message, not a plausible-looking
// report in the wrong currency.
func TestReportRequiresCurrency(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			status, body, _ := e.get("/api/v1/reports/trial_balance")
			if status != http.StatusBadRequest {
				t.Errorf("report without currency = %d, want 400; body=%s", status, body)
			}
		})
	}
}

func TestUnknownReportIs404(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if status, body, _ := e.get("/api/v1/reports/does_not_exist?currency=EUR"); status != http.StatusNotFound {
				t.Errorf("unknown report = %d, want 404; body=%s", status, body)
			}
			if status, body, _ := e.get("/api/v1/metrics/does_not_exist?currency=EUR"); status != http.StatusNotFound {
				t.Errorf("unknown metric = %d, want 404; body=%s", status, body)
			}
		})
	}
}

// Metrics evaluate over HTTP and agree with the report that shows the same
// number — the docs/21 §1 guarantee, checked at the edge.
func TestMetricsOverHTTP(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, gross := booksOverHTTP(t, e)

			status, body, all := e.get("/api/v1/metrics?currency=EUR&as_of=2026-07-31")
			if status != http.StatusOK {
				t.Fatalf("list metrics = %d; body=%s", status, body)
			}
			values, _ := all["data"].([]any)
			if len(values) == 0 {
				t.Fatal("no metrics evaluated for a fully-privileged caller")
			}

			status, body, one := e.get("/api/v1/metrics/ar_outstanding?currency=EUR&as_of=2026-07-31")
			if status != http.StatusOK {
				t.Fatalf("evaluate metric = %d; body=%s", status, body)
			}
			got, _ := one["value"].(float64)
			if want := gross - 50000; got != want {
				t.Errorf("ar_outstanding = %v, want %v", got, want)
			}
		})
	}
}

// Export produces a real file with the right content type, and — importantly —
// runs through the same permission gate as the JSON route.
func TestReportExportOverHTTP(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			booksOverHTTP(t, e)

			// CSV.
			status, raw, _ := e.get("/api/v1/reports/trial_balance/export?currency=EUR&format=csv")
			if status != http.StatusOK {
				t.Fatalf("csv export = %d; body=%s", status, raw)
			}
			records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
			if err != nil {
				t.Fatalf("exported CSV does not parse: %v", err)
			}
			if len(records) < 3 {
				t.Errorf("CSV export has %d records, want a title, header and rows", len(records))
			}

			// XLSX.
			status, raw, _ = e.get("/api/v1/reports/trial_balance/export?currency=EUR&format=xlsx")
			if status != http.StatusOK {
				t.Fatalf("xlsx export = %d; body=%s", status, raw)
			}
			if _, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw))); err != nil {
				t.Errorf("xlsx export is not a valid workbook: %v", err)
			}

			// Unsupported format is a 400, not a silent CSV.
			if status, body, _ := e.get("/api/v1/reports/trial_balance/export?currency=EUR&format=pdf"); status != http.StatusBadRequest {
				t.Errorf("unsupported export format = %d, want 400; body=%s", status, body)
			}
		})
	}
}

// INV-T1 at the edge: a caller who cannot read the underlying object gets a 403
// from the report route, and the export route is not a way around it.
func TestReportRoutesRefuseUnderprivilegedCaller(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			booksOverHTTP(t, e)

			// A session with no report-relevant grants at all.
			limited := e.issueUser(t, map[string][]string{"capability": {"manage"}})

			for _, path := range []string{
				"/api/v1/reports/trial_balance?currency=EUR",
				"/api/v1/reports/ar_aging?currency=EUR",
				"/api/v1/reports/trial_balance/export?currency=EUR&format=csv",
				"/api/v1/reports/trial_balance/export?currency=EUR&format=xlsx",
				"/api/v1/metrics/ar_outstanding?currency=EUR",
			} {
				status, body, _ := e.call("GET", path, limited, "", nil)
				if status != http.StatusForbidden {
					t.Errorf("%s for an under-privileged caller = %d, want 403; body=%s", path, status, body)
				}
				// Nothing resembling a figure should appear in the refusal.
				if strings.Contains(string(body), "amount_minor") {
					t.Errorf("%s leaked report data in its refusal: %s", path, body)
				}
			}

			// The metric list returns an empty set rather than refusing outright:
			// the catalog is not secret, the values are.
			status, body, all := e.call("GET", "/api/v1/metrics?currency=EUR", limited, "", nil)
			if status != http.StatusOK {
				t.Fatalf("metric list = %d; body=%s", status, body)
			}
			if values, _ := all["data"].([]any); len(values) != 0 {
				t.Errorf("metric list returned %d values to an under-privileged caller", len(values))
			}
		})
	}
}

// Reports are documented, so a generated client can call them.
func TestReportRoutesAreInOpenAPI(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			status, body, spec := e.get("/api/v1/openapi.json")
			if status != http.StatusOK {
				t.Fatalf("openapi = %d; body=%s", status, body)
			}
			paths, _ := spec["paths"].(map[string]any)
			for _, p := range []string{
				"/api/v1/reports", "/api/v1/reports/{name}",
				"/api/v1/reports/{name}/export", "/api/v1/metrics", "/api/v1/metrics/{name}",
			} {
				if _, ok := paths[p]; !ok {
					t.Errorf("OpenAPI is missing report path %q", p)
				}
			}
		})
	}
}

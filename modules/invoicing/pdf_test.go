// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"bytes"
	"strings"
	"testing"
)

// TestRenderInvoicePDF checks the hand-rolled writer emits a structurally valid
// PDF (header, xref, trailer, EOF) that carries the invoice number and totals.
func TestRenderInvoicePDF(t *testing.T) {
	inv := Invoice{
		ID: "inv-1", ContactID: "acme", Currency: "EUR", Status: StatusPosted,
		Number: "INV-000007", IssueDate: "2026-01-15",
		ARAccount: "ar", TaxAccount: "tax",
		Lines: []Line{
			{Description: "Consulting (Q1)", Quantity: 2, UnitPriceMinor: 10000, RevenueAccount: "rev"},
		},
		NetMinor: 20000, TaxMinor: 4000, GrossMinor: 24000,
	}
	pdf, err := RenderInvoicePDF(inv)
	if err != nil {
		t.Fatalf("RenderInvoicePDF: %v", err)
	}

	if !bytes.HasPrefix(pdf, []byte("%PDF-1.")) {
		t.Fatalf("missing PDF header, got %q", pdf[:min(16, len(pdf))])
	}
	s := string(pdf)
	for _, want := range []string{"xref", "trailer", "/Root 1 0 R", "startxref", "%%EOF"} {
		if !strings.Contains(s, want) {
			t.Errorf("PDF missing %q", want)
		}
	}
	// The number and the total must be rendered into the content stream.
	if !strings.Contains(s, "INV-000007") {
		t.Error("PDF does not contain the invoice number")
	}
	if !strings.Contains(s, "240.00 EUR") {
		t.Error("PDF does not contain the gross total")
	}
	// startxref must point at the 'xref' keyword.
	xrefOff := strings.Index(s, "\nxref\n") + 1
	declared := trailerStartxref(t, s)
	if declared != xrefOff {
		t.Errorf("startxref = %d, but 'xref' is at %d", declared, xrefOff)
	}
}

// A draft (no number) still renders without error.
func TestRenderDraftPDF(t *testing.T) {
	inv := Invoice{ID: "d1", Currency: "USD", Status: StatusDraft, IssueDate: "2026-02-01",
		Lines: []Line{{Description: "x", Quantity: 1, UnitPriceMinor: 500, RevenueAccount: "r"}}}
	pdf, err := RenderInvoicePDF(inv)
	if err != nil {
		t.Fatalf("RenderInvoicePDF draft: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatal("draft PDF missing header")
	}
}

// pdfEscape must neutralise the string delimiters so a description with
// parentheses cannot corrupt the content stream.
func TestPDFEscape(t *testing.T) {
	if got := pdfEscape("a (b) \\ c"); got != `a \(b\) \\ c` {
		t.Fatalf("pdfEscape = %q", got)
	}
}

func trailerStartxref(t *testing.T, s string) int {
	t.Helper()
	const key = "startxref\n"
	i := strings.LastIndex(s, key)
	if i < 0 {
		t.Fatal("no startxref")
	}
	rest := s[i+len(key):]
	end := strings.IndexByte(rest, '\n')
	n := 0
	for _, c := range rest[:end] {
		n = n*10 + int(c-'0')
	}
	return n
}

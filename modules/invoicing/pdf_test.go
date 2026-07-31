// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/text/language"

	"github.com/iamdoubz/lasterp/kernel/i18n"
)

// testPrinter is the document printer for a locale, built from the real packs —
// a test that stubbed the catalog would prove nothing about the shipped one.
func testPrinter(t *testing.T, locale string) *i18n.Printer {
	t.Helper()
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("i18n.Load: %v", err)
	}
	return tr.Printer(language.MustParse(locale))
}

func samplePostedInvoice() Invoice {
	return Invoice{
		ID: "inv-1", ContactID: "acme", Currency: "EUR", Status: StatusPosted,
		Number: "INV-000007", IssueDate: "2026-01-15",
		ARAccount: "ar", TaxAccount: "tax",
		Lines: []Line{{
			Description:     "Rocket-powered roller skates",
			DescriptionI18n: i18n.Localized{"de": "Raketenrollschuhe"},
			Quantity:        2, UnitPriceMinor: 10000, RevenueAccount: "rev",
		}},
		NetMinor: 20000, TaxMinor: 4000, GrossMinor: 24000,
	}
}

// TestRenderInvoicePDF checks the hand-rolled writer emits a structurally valid
// PDF (header, xref, trailer, EOF) that carries the invoice number and totals.
func TestRenderInvoicePDF(t *testing.T) {
	pdf, err := RenderInvoicePDF(samplePostedInvoice(), testPrinter(t, "en"))
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
	for _, want := range []string{"Invoice", "Issue date", "2026-01-15", "Total", "240.00"} {
		if !strings.Contains(s, want) {
			t.Errorf("English PDF does not contain %q", want)
		}
	}
	// startxref must point at the 'xref' keyword.
	xrefOff := strings.Index(s, "\nxref\n") + 1
	declared := trailerStartxref(t, s)
	if declared != xrefOff {
		t.Errorf("startxref = %d, but 'xref' is at %d", declared, xrefOff)
	}
}

// The WP-1.7 AC in one test: the same document, rendered in the counterparty's
// language — labels, date order, decimal mark and the line's own text.
func TestRenderInvoicePDFInGerman(t *testing.T) {
	pdf, err := RenderInvoicePDF(samplePostedInvoice(), testPrinter(t, "de"))
	if err != nil {
		t.Fatalf("RenderInvoicePDF: %v", err)
	}
	s := string(pdf)

	for _, want := range []string{
		"Rechnung",          // the title, from the pack
		"Rechnungsdatum",    // a label, from the pack
		"15.01.2026",        // the German date order, from the pack's pattern
		"240,00",            // the German decimal mark
		"Raketenrollschuhe", // the line's per-locale description
		"Gesamtbetrag",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("German PDF does not contain %q", want)
		}
	}
	for _, unwanted := range []string{"Issue date", "Total:", "Rocket-powered roller skates"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("German PDF still contains the English %q", unwanted)
		}
	}
}

// Umlauts must reach the page as Windows-1252 bytes against a font that
// declares that encoding. Emitting UTF-8 into a StandardEncoding Type1 font is
// how "Währung" becomes "WÃ¤hrung" — or vanishes.
func TestGermanPDFEncodesUmlautsAsWinAnsi(t *testing.T) {
	pdf, err := RenderInvoicePDF(samplePostedInvoice(), testPrinter(t, "de"))
	if err != nil {
		t.Fatalf("RenderInvoicePDF: %v", err)
	}

	if !bytes.Contains(pdf, []byte("/Encoding /WinAnsiEncoding")) {
		t.Error("font dictionary does not declare WinAnsiEncoding")
	}
	// "Währung" — ä is 0xE4 in Windows-1252, two bytes (0xC3 0xA4) in UTF-8.
	if !bytes.Contains(pdf, []byte{'W', 0xE4, 'h', 'r', 'u', 'n', 'g'}) {
		t.Error("umlaut was not transcoded to Windows-1252")
	}
	if bytes.Contains(pdf, []byte("Währung")) {
		t.Error("PDF carries raw UTF-8 text, which WinAnsiEncoding will misrender")
	}
}

// A rune outside Windows-1252 must degrade to "?" rather than emit bytes the
// reader would render as a different letter.
func TestPDFReplacesUnencodableRunes(t *testing.T) {
	inv := samplePostedInvoice()
	inv.Lines[0].Description = "Ракета"
	inv.Lines[0].DescriptionI18n = nil

	pdf, err := RenderInvoicePDF(inv, testPrinter(t, "en"))
	if err != nil {
		t.Fatalf("RenderInvoicePDF: %v", err)
	}
	if bytes.Contains(pdf, []byte("Ракета")) {
		t.Error("PDF carries raw UTF-8 for a rune Windows-1252 cannot represent")
	}
	if !bytes.Contains(pdf, []byte("??????")) {
		t.Error("unencodable runes were dropped instead of replaced")
	}
}

// The server-side counterpart of the hardcoded-string lint gate, deferred here
// by WP-0.7: rendered under the pseudo-locale every label comes back accented,
// so any English still on the page is a string that never went through the
// catalog. Asserting the *absence* of each source string makes the check
// automatic — a label added later without a pack key fails without anyone
// remembering to extend this test.
func TestInvoicePDFHasNoUnexternalizedStrings(t *testing.T) {
	packs, err := i18n.BuiltinPacks()
	if err != nil {
		t.Fatalf("BuiltinPacks: %v", err)
	}

	inv := samplePostedInvoice()
	// Document *data* is not translated, so it must not collide with a label.
	inv.Lines[0].Description = "ZZZ-line"
	inv.Lines[0].DescriptionI18n = nil
	inv.ContactID = "ZZZ-contact"

	pdf, err := RenderInvoicePDF(inv, testPrinter(t, i18n.PseudoAccented.String()))
	if err != nil {
		t.Fatalf("RenderInvoicePDF: %v", err)
	}
	rendered := string(pdf)

	for _, pack := range packs {
		if pack.Locale != i18n.SourceLocale {
			continue
		}
		for key, source := range pack.Messages {
			if key == "doc.date.pattern" {
				continue // a formatting rule, deliberately not accented
			}
			if strings.Contains(rendered, pdfEscape(source)) {
				t.Errorf("pseudo-localized PDF still shows the source string %q (%s) — it did not go through the catalog", source, key)
			}
		}
	}
}

// A draft (no number) still renders, and says so in the reader's language.
func TestRenderDraftPDF(t *testing.T) {
	inv := Invoice{ID: "d1", Currency: "USD", Status: StatusDraft, IssueDate: "2026-02-01",
		Lines: []Line{{Description: "x", Quantity: 1, UnitPriceMinor: 500, RevenueAccount: "r"}}}

	pdf, err := RenderInvoicePDF(inv, testPrinter(t, "de"))
	if err != nil {
		t.Fatalf("RenderInvoicePDF draft: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatal("draft PDF missing header")
	}
	if !strings.Contains(string(pdf), "Entwurf") {
		t.Error("German draft PDF does not carry the localized draft marker")
	}
}

// pdfEscape must neutralise the string delimiters so a description with
// parentheses cannot corrupt the content stream.
func TestPDFEscape(t *testing.T) {
	if got := pdfEscape("a (b) \\ c"); got != `a \(b\) \\ c` {
		t.Fatalf("pdfEscape = %q", got)
	}
}

// Column padding counts characters, not bytes: an 18-character German label is
// 19 bytes, and padding by bytes shifts every column after it.
func TestPaddingCountsRunesNotBytes(t *testing.T) {
	if got := len([]rune(padRight("Rechnungsempfänger", 30))); got != 30 {
		t.Errorf("padRight produced %d runes, want 30", got)
	}
	if got := len([]rune(padLeft("€ 1.000,00", 14))); got != 14 {
		t.Errorf("padLeft produced %d runes, want 14", got)
	}
	if got := truncate("Raketenrollschuhe", 5); got != "Raket" {
		t.Errorf("truncate = %q", got)
	}
	// Truncation must not split a multi-byte rune in half.
	if got := truncate("Wärmepumpe", 2); got != "Wä" {
		t.Errorf("truncate on a multi-byte rune = %q, want %q", got, "Wä")
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

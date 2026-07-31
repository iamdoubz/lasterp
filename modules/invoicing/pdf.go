// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"

	"github.com/iamdoubz/lasterp/kernel/i18n"
)

// RenderInvoicePDF renders inv as a single-page PDF in the language p prints,
// and returns the bytes. Every visible string comes from p — labels from the
// translation pack, amounts and the issue date from the locale's formatting
// rules, line text from the line's own per-locale values (docs/17: "documents
// render in the counterparty's language"). Nothing here decides *which* locale
// that is; the caller resolves it, so this stays a pure function of (document,
// printer) that a test can render twice and compare.
//
// ponytail: a minimal hand-rolled PDF writer (stdlib + x/text) — no PDF
// dependency (which would need an ADR) for what is a simple text container. It
// emits a valid PDF 1.4 (catalog → pages → one page → Helvetica → content
// stream, with a correct xref table + trailer) that any reader opens. A shared
// kernel PDF / template-pack service is the upgrade path once a second document
// type (payslip) needs one; until then this lives in the one module that
// renders.
func RenderInvoicePDF(inv Invoice, p *i18n.Printer) ([]byte, error) {
	content, err := invoiceContentStream(inv, p)
	if err != nil {
		return nil, err
	}

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		// WinAnsiEncoding, not the base font's built-in StandardEncoding, which
		// has no ä/ö/ü/ß — a German invoice printed "W hrung" without this.
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xrefStart)

	return buf.Bytes(), nil
}

// invoiceContentStream builds the page's text drawing operators.
func invoiceContentStream(inv Invoice, p *i18n.Printer) (string, error) {
	number := inv.Number
	if number == "" {
		number = p.T("doc.invoice.draft")
	}
	money := func(minor int64) (string, error) { return p.Money(minor, inv.Currency) }

	var b strings.Builder
	b.WriteString("BT\n")
	fmt.Fprintf(&b, "/F1 18 Tf\n50 750 Td\n(%s) Tj\n", pdfText(p.T("doc.invoice.title")+" "+number))
	b.WriteString("/F1 10 Tf\n")
	line := func(s string) { fmt.Fprintf(&b, "0 -16 Td\n(%s) Tj\n", pdfText(s)) }
	labelled := func(key, value string) { line(p.T(key) + ": " + value) }

	labelled("doc.invoice.issueDate", p.Date(inv.IssueDate))
	labelled("doc.invoice.billTo", inv.ContactID)
	labelled("doc.invoice.currency", inv.Currency)
	line("")
	line(columns(
		p.T("doc.invoice.column.description"),
		p.T("doc.invoice.column.quantity"),
		p.T("doc.invoice.column.unitPrice"),
		p.T("doc.invoice.column.net"),
	))
	for _, l := range inv.Lines {
		unit, err := money(l.UnitPriceMinor)
		if err != nil {
			return "", err
		}
		net, err := money(l.netMinor())
		if err != nil {
			return "", err
		}
		line(columns(l.DescriptionFor(p.Tag()), p.Number(l.Quantity), unit, net))
	}
	line("")
	for _, total := range []struct {
		key   string
		minor int64
	}{
		{"doc.invoice.total.net", inv.NetMinor},
		{"doc.invoice.total.tax", inv.TaxMinor},
		{"doc.invoice.total.gross", inv.GrossMinor},
	} {
		amount, err := money(total.minor)
		if err != nil {
			return "", err
		}
		labelled(total.key, amount)
	}
	b.WriteString("ET")
	return b.String(), nil
}

// columns lays the line table out in fixed-width fields. Widths count runes,
// not bytes: "Rechnungsempfänger" is 18 characters and 19 bytes, and padding by
// the latter shifts every column after it.
//
// ponytail: Helvetica is proportional, so this is approximate alignment, not
// typesetting. Real column positions need per-column Td offsets, which is the
// template-pack service's job, not a text-only document's.
func columns(description, quantity, unitPrice, net string) string {
	return padRight(description, 30) + " " + padLeft(quantity, 5) + " " +
		padLeft(unitPrice, 14) + " " + padLeft(net, 14)
}

func padRight(s string, width int) string {
	s = truncate(s, width)
	return s + strings.Repeat(" ", width-len([]rune(s)))
}

func padLeft(s string, width int) string {
	s = truncate(s, width)
	return strings.Repeat(" ", width-len([]rune(s))) + s
}

// pdfText prepares a Go string for a PDF literal: transcoded to the font's
// WinAnsi (Windows-1252) encoding, then escaped.
//
// ponytail: Windows-1252 covers Latin-1 — de, es, fr, it, nl, pt, the Nordics.
// Greek, Cyrillic, Turkish and CJK need an embedded TrueType font with a CID
// encoding (the template-pack service above); unmappable runes become "?"
// rather than bytes that would render as a different letter.
func pdfText(s string) string {
	encoded, _, err := transform.String(encoding.ReplaceUnsupported(winAnsi.NewEncoder()), s)
	if err != nil {
		// The replacing encoder has no failure mode left; fall back to the raw
		// string rather than dropping the whole document over one glyph.
		encoded = s
	}
	// x/text substitutes SUB (0x1A) for what it cannot encode, which a PDF
	// reader draws as nothing at all — a silently shortened word. "?" at least
	// shows the reader that a character is missing.
	return pdfEscape(strings.ReplaceAll(encoded, string(encoding.ASCIISub), "?"))
}

var winAnsi = charmap.Windows1252

// pdfEscape escapes the characters that are special inside a PDF literal string.
func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`, "\n", " ", "\r", " ")
	return r.Replace(s)
}

// truncate cuts s to at most n runes — never mid-rune, which would emit half a
// UTF-8 sequence into the encoder.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/money"
)

// RenderInvoicePDF renders inv as a single-page PDF and returns the bytes.
//
// ponytail: a minimal hand-rolled PDF writer (stdlib only) — no PDF dependency
// (which would need an ADR) for what is a simple text container. It emits a
// valid PDF 1.4 (catalog → pages → one page → Helvetica → content stream, with
// a correct xref table + trailer) that any reader opens. A shared kernel PDF /
// template-pack service is the upgrade path once a second document type
// (payslip) needs one; until then this lives in the one module that renders.
func RenderInvoicePDF(inv Invoice) ([]byte, error) {
	content, err := invoiceContentStream(inv)
	if err != nil {
		return nil, err
	}

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
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
func invoiceContentStream(inv Invoice) (string, error) {
	number := inv.Number
	if number == "" {
		number = "(draft)"
	}
	var b strings.Builder
	b.WriteString("BT\n")
	fmt.Fprintf(&b, "/F1 18 Tf\n50 750 Td\n(%s) Tj\n", pdfEscape("Invoice "+number))
	b.WriteString("/F1 10 Tf\n")
	line := func(s string) { fmt.Fprintf(&b, "0 -16 Td\n(%s) Tj\n", pdfEscape(s)) }

	line("Issue date: " + inv.IssueDate)
	line("Bill to (contact): " + inv.ContactID)
	line("Currency: " + inv.Currency)
	line("")
	line("Description                 Qty     Unit        Net")
	for _, l := range inv.Lines {
		unit, err := money.New(l.UnitPriceMinor, inv.Currency)
		if err != nil {
			return "", err
		}
		net, err := money.New(l.netMinor(), inv.Currency)
		if err != nil {
			return "", err
		}
		line(fmt.Sprintf("%-26s %5d  %10s %10s",
			truncate(l.Description, 26), l.Quantity, unit.String(), net.String()))
	}
	line("")
	if net, err := money.New(inv.NetMinor, inv.Currency); err == nil {
		line("Net:   " + net.String())
	}
	if tax, err := money.New(inv.TaxMinor, inv.Currency); err == nil {
		line("Tax:   " + tax.String())
	}
	if gross, err := money.New(inv.GrossMinor, inv.Currency); err == nil {
		line("Total: " + gross.String())
	}
	b.WriteString("ET")
	return b.String(), nil
}

// pdfEscape escapes the characters that are special inside a PDF literal string.
func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`, "\n", " ", "\r", " ")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

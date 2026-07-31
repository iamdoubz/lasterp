// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"strconv"
)

// Report export. CSV is encoding/csv; XLSX is a ZIP of XML parts, so
// archive/zip + encoding/xml cover it — both stdlib.
//
// The XLSX writer is hand-rolled for the same reason WP-1.4's PDF writer was:
// a spreadsheet library is a large dependency needing an ADR, and the scope
// here is one worksheet with a header row and typed cells. If real formatting
// requirements ever arrive, that ADR is the right conversation to have then
// (WP-1.6-decisions.md §7).

// ExportCSV renders a report as CSV. Money is written as a decimal string built
// from integer minor units — never a float, and never a spreadsheet-mangled
// value (INV-F4).
func ExportCSV(rep Report) ([]byte, error) {
	// Every record is the same width. A ragged CSV — a one-field title row above
	// three-field data rows — is rejected outright by encoding/csv's reader and
	// by strict spreadsheet importers, so the banner rows are padded rather than
	// written short.
	const width = 3
	pad := func(fields ...string) []string {
		row := make([]string, width)
		copy(row, fields)
		return row
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	records := [][]string{pad(rep.Title)}
	if rep.Basis != "" {
		records = append(records, pad(rep.Basis))
	}
	records = append(records, pad("Label", "Amount", "Currency"))
	for _, r := range rep.Rows {
		records = append(records, pad(r.Label, minorToDecimal(r.AmountMinor), r.Currency))
	}
	if len(rep.Totals) > 0 {
		records = append(records, pad())
		for _, r := range rep.Totals {
			records = append(records, pad(r.Label, minorToDecimal(r.AmountMinor), r.Currency))
		}
	}
	if err := w.WriteAll(records); err != nil {
		return nil, err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// minorToDecimal renders integer minor units as an exact decimal string.
//
// ponytail: fixed 2 decimal places. Correct for the overwhelming majority of
// ISO-4217 currencies but wrong for JPY (0) and TND (3); the exponent lives in
// kernel/money's registry, and threading it through Report is the upgrade when
// a zero- or three-decimal currency actually needs exporting. Integer arithmetic
// throughout, so no float ever touches the value.
func minorToDecimal(minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	units := minor / 100
	frac := minor % 100
	s := strconv.FormatInt(units, 10) + "." + fmt.Sprintf("%02d", frac)
	if neg {
		return "-" + s
	}
	return s
}

// --- XLSX ---

// ExportXLSX renders a report as a minimal single-sheet .xlsx workbook.
func ExportXLSX(rep Report) ([]byte, error) {
	rows := [][]cell{
		{textCell(rep.Title)},
	}
	if rep.Basis != "" {
		rows = append(rows, []cell{textCell(rep.Basis)})
	}
	rows = append(rows, []cell{textCell("Label"), textCell("Amount"), textCell("Currency")})
	for _, r := range rep.Rows {
		rows = append(rows, []cell{textCell(r.Label), numberCell(r.AmountMinor), textCell(r.Currency)})
	}
	if len(rep.Totals) > 0 {
		rows = append(rows, nil) // blank separator row
		for _, r := range rep.Totals {
			rows = append(rows, []cell{textCell(r.Label), numberCell(r.AmountMinor), textCell(r.Currency)})
		}
	}

	sheet, err := sheetXML(rows)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := []struct {
		name    string
		content string
	}{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", rootRelsXML},
		{"xl/workbook.xml", workbookXML},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML},
		{"xl/worksheets/sheet1.xml", sheet},
	}
	for _, p := range parts {
		w, err := zw.Create(p.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(p.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cell is one worksheet cell: either inline text or a number.
type cell struct {
	text   string
	number string
	isText bool
}

func textCell(s string) cell { return cell{text: s, isText: true} }

// numberCell writes money as a real number so the spreadsheet can sum it. The
// decimal string is built with integer arithmetic and handed to XML as text —
// no float is ever constructed on this path.
func numberCell(minor int64) cell { return cell{number: minorToDecimal(minor)} }

// sheetXML renders the worksheet part. Cell references are A1-style; only
// columns A–Z are emitted, which is ample for a three-column report and avoids
// hand-rolling base-26 arithmetic that nothing here needs.
func sheetXML(rows [][]cell) (string, error) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	// Freeze the header so a long report stays readable when scrolled.
	b.WriteString(`<sheetViews><sheetView workbookViewId="0">` +
		`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>` +
		`</sheetView></sheetViews>`)
	b.WriteString(`<sheetData>`)
	for i, row := range rows {
		rowNum := i + 1
		b.WriteString(`<row r="` + strconv.Itoa(rowNum) + `">`)
		for j, c := range row {
			if j >= 26 {
				return "", fmt.Errorf("reporting: xlsx export supports at most 26 columns, got %d", len(row))
			}
			ref := string(rune('A'+j)) + strconv.Itoa(rowNum)
			if c.isText {
				escaped, err := escapeXML(c.text)
				if err != nil {
					return "", err
				}
				b.WriteString(`<c r="` + ref + `" t="inlineStr"><is><t xml:space="preserve">` + escaped + `</t></is></c>`)
				continue
			}
			b.WriteString(`<c r="` + ref + `"><v>` + c.number + `</v></c>`)
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String(), nil
}

// escapeXML escapes a value for XML character data. Report labels carry
// tenant-authored text (account and contact names), so this is a real injection
// boundary, not a formality: an unescaped "&" or "<" produces a workbook Excel
// refuses to open, and worse shapes are conceivable.
func escapeXML(s string) (string, error) {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// The static OOXML parts. Minimal but complete: Excel and LibreOffice both
// require all four to consider the archive a valid workbook.
const (
	contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
		`</Types>`

	rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
		`</Relationships>`

	workbookXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="Report" sheetId="1" r:id="rId1"/></sheets></workbook>`

	workbookRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
		`</Relationships>`
)

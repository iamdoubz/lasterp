// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func exportFixture() Report {
	return Report{
		Name: "trial_balance", Title: "Trial balance", Currency: "EUR",
		Columns: []string{"Account", "Debit", "Credit"},
		Rows: []Row{
			{Label: "1000 Bank", Key: "1000", Currency: "EUR", AmountMinor: 5700000},
			{Label: "4000 Revenue", Key: "4000", Currency: "EUR", AmountMinor: -10000000},
		},
		Totals: []Row{{Label: "Total debits", Key: "debits", Currency: "EUR", AmountMinor: 10000000}},
	}
}

// --- money rendering ---

// Money is exported as an exact decimal built from integer minor units. A float
// anywhere on this path is a misstated figure in someone's spreadsheet.
func TestMinorToDecimal(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{7, "0.07"}, // the classic float round-trip casualty
		{99, "0.99"},
		{100, "1.00"},
		{123456, "1234.56"},
		{-1, "-0.01"},
		{-123456, "-1234.56"},
		{-100, "-1.00"},
	}
	for _, tc := range tests {
		if got := minorToDecimal(tc.minor); got != tc.want {
			t.Errorf("minorToDecimal(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

// Round-tripping the decimal string back to minor units is exact for every
// value — the property that makes the export trustworthy.
func TestMinorToDecimalRoundTripsExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 5000; i++ {
		minor := rng.Int63n(200_000_000) - 100_000_000
		s := minorToDecimal(minor)
		whole, frac, ok := strings.Cut(s, ".")
		if !ok || len(frac) != 2 {
			t.Fatalf("minorToDecimal(%d) = %q, not a 2-decimal string", minor, s)
		}
		neg := strings.HasPrefix(whole, "-")
		units, err := strconv.ParseInt(strings.TrimPrefix(whole, "-"), 10, 64)
		if err != nil {
			t.Fatalf("unparseable units in %q: %v", s, err)
		}
		cents, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			t.Fatalf("unparseable cents in %q: %v", s, err)
		}
		got := units*100 + cents
		if neg {
			got = -got
		}
		if got != minor {
			t.Fatalf("round trip of %d via %q gave %d", minor, s, got)
		}
	}
}

// --- CSV ---

func TestExportCSVIsParseable(t *testing.T) {
	out, err := ExportCSV(exportFixture())
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	records, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("exported CSV does not parse: %v", err)
	}
	if len(records) < 4 {
		t.Fatalf("CSV has %d records, want title + header + 2 rows at least", len(records))
	}
	if records[0][0] != "Trial balance" {
		t.Errorf("first row = %v, want the report title", records[0])
	}
	// Amounts are exact decimals, not floats.
	var found bool
	for _, r := range records {
		if len(r) >= 2 && r[0] == "1000 Bank" {
			found = true
			if r[1] != "57000.00" {
				t.Errorf("bank amount = %q, want 57000.00", r[1])
			}
		}
	}
	if !found {
		t.Error("row not found in CSV output")
	}
}

// A label containing a comma or quote must not corrupt the file — account and
// contact names are tenant-authored.
func TestExportCSVEscapesHostileLabels(t *testing.T) {
	rep := exportFixture()
	rep.Rows = []Row{{
		Label: `Acme, "Inc" ` + "\n" + `and friends`, Key: "x", Currency: "EUR", AmountMinor: 100,
	}}
	out, err := ExportCSV(rep)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	records, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("hostile label broke the CSV: %v", err)
	}
	var found bool
	for _, r := range records {
		if len(r) >= 1 && r[0] == rep.Rows[0].Label {
			found = true
		}
	}
	if !found {
		t.Errorf("label did not survive the round trip; got %v", records)
	}
}

// --- XLSX ---

// The workbook must be a valid ZIP containing the four OOXML parts Excel and
// LibreOffice both require, and the sheet must be well-formed XML.
func TestExportXLSXIsAValidWorkbook(t *testing.T) {
	out, err := ExportXLSX(exportFixture())
	if err != nil {
		t.Fatalf("ExportXLSX: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}

	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		parts[f.Name] = string(b)
	}

	for _, required := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml",
	} {
		if _, ok := parts[required]; !ok {
			t.Errorf("workbook is missing required part %q", required)
		}
	}

	// Every part must be well-formed XML, or the file simply will not open.
	for name, content := range parts {
		if err := xml.Unmarshal([]byte(content), new(any)); err != nil {
			// xml.Unmarshal into any is lenient about structure but strict about
			// well-formedness, which is what we care about here.
			d := xml.NewDecoder(strings.NewReader(content))
			for {
				_, terr := d.Token()
				if errors.Is(terr, io.EOF) {
					break
				}
				if terr != nil {
					t.Errorf("part %s is not well-formed XML: %v", name, terr)
					break
				}
			}
		}
	}

	sheet := parts["xl/worksheets/sheet1.xml"]
	if !strings.Contains(sheet, "Trial balance") {
		t.Error("sheet does not contain the report title")
	}
	// Money lands in a numeric cell (no t="..." attribute), so the spreadsheet
	// can sum it rather than treating it as text.
	if !strings.Contains(sheet, `<v>57000.00</v>`) {
		t.Errorf("bank amount is not a numeric cell; sheet=%s", sheet)
	}
	if !strings.Contains(sheet, `state="frozen"`) {
		t.Error("header row is not frozen")
	}
}

// Tenant-authored labels reach the XML directly, so this is a real injection
// boundary: an unescaped & or < produces a workbook that will not open.
func TestExportXLSXEscapesHostileLabels(t *testing.T) {
	rep := exportFixture()
	rep.Rows = []Row{{
		Label: `Ampersand & <tag> "quoted" 'single'`, Key: "x", Currency: "EUR", AmountMinor: 1,
	}}
	out, err := ExportXLSX(rep)
	if err != nil {
		t.Fatalf("ExportXLSX: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "xl/worksheets/sheet1.xml" {
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()

		// Well-formedness is the whole point of the escaping.
		d := xml.NewDecoder(bytes.NewReader(b))
		for {
			_, err := d.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("hostile label produced malformed XML: %v", err)
			}
		}
		if bytes.Contains(b, []byte("<tag>")) {
			t.Error("raw markup from a label reached the sheet unescaped")
		}
		return
	}
	t.Fatal("sheet part not found")
}

// A report wider than the column scheme supports must fail loudly rather than
// emit a corrupt workbook.
func TestExportXLSXRefusesTooManyColumns(t *testing.T) {
	wide := make([]cell, 27)
	for i := range wide {
		wide[i] = textCell("x")
	}
	if _, err := sheetXML([][]cell{wide}); err == nil {
		t.Fatal("expected a refusal for a 27-column row")
	}
}

// An empty report still exports something openable rather than erroring.
func TestExportEmptyReport(t *testing.T) {
	empty := Report{Name: "ar_aging", Title: "AR aging", Currency: "EUR"}
	if _, err := ExportCSV(empty); err != nil {
		t.Errorf("ExportCSV on an empty report: %v", err)
	}
	out, err := ExportXLSX(empty)
	if err != nil {
		t.Fatalf("ExportXLSX on an empty report: %v", err)
	}
	if _, err := zip.NewReader(bytes.NewReader(out), int64(len(out))); err != nil {
		t.Errorf("empty workbook is not a valid zip: %v", err)
	}
}

// The basis line must survive export: an AR aging read as due-date aging when
// it is issue-date aging is a materially wrong conclusion.
func TestExportCarriesBasis(t *testing.T) {
	rep := exportFixture()
	rep.Basis = agingBasis

	csvOut, err := ExportCSV(rep)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !bytes.Contains(csvOut, []byte("issue date")) {
		t.Error("CSV export dropped the basis line")
	}
	xlsxOut, err := ExportXLSX(rep)
	if err != nil {
		t.Fatalf("ExportXLSX: %v", err)
	}
	if !bytes.Contains(xlsxOut, []byte("PK")) {
		t.Error("xlsx output is not a zip")
	}
}

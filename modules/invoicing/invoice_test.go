// SPDX-License-Identifier: AGPL-3.0-only

// Pure (no-DB) unit tests for the invoicing posting template and draft
// validation. The DB-backed invariant suite is invoice_integrity_test.go.
package invoicing

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/modules/tax"
)

func mustMoney(t *testing.T, minor int64, code string) money.Money {
	t.Helper()
	m, err := money.New(minor, code)
	if err != nil {
		t.Fatalf("money.New(%d,%q): %v", minor, code, err)
	}
	return m
}

// INV-F5/INV-F1: the declared template maps an invoice + resolved tax to a
// balanced journal — DR AR gross, CR each revenue account its net, CR tax the
// total tax — and the totals it reports match.
func TestBuildInvoiceJournalBalances(t *testing.T) {
	inv := Invoice{
		Currency: "EUR", ARAccount: "ar", TaxAccount: "taxpay",
		Lines: []Line{
			{RevenueAccount: "rev-a", Quantity: 1, UnitPriceMinor: 10000},
			{RevenueAccount: "rev-b", Quantity: 1, UnitPriceMinor: 5000},
			{RevenueAccount: "rev-a", Quantity: 1, UnitPriceMinor: 2000}, // same account as line 0
		},
	}
	taxLines := []tax.LineResult{
		{Net: mustMoney(t, 10000, "EUR"), Tax: mustMoney(t, 2000, "EUR"), Gross: mustMoney(t, 12000, "EUR")},
		{Net: mustMoney(t, 5000, "EUR"), Tax: mustMoney(t, 1000, "EUR"), Gross: mustMoney(t, 6000, "EUR")},
		{Net: mustMoney(t, 2000, "EUR"), Tax: mustMoney(t, 400, "EUR"), Gross: mustMoney(t, 2400, "EUR")},
	}

	cmd, totals, err := buildInvoiceJournal(inv, taxLines, "2026-01", "cmd-x")
	if err != nil {
		t.Fatalf("buildInvoiceJournal: %v", err)
	}

	var debits, credits int64
	byAccount := map[string]int64{}
	for _, l := range cmd.Lines {
		debits += l.Debit
		credits += l.Credit
		byAccount[l.AccountID] += l.Debit - l.Credit
	}
	if debits != credits {
		t.Fatalf("journal does not balance: debits=%d credits=%d", debits, credits)
	}
	// AR debited the gross.
	if byAccount["ar"] != 20400 {
		t.Fatalf("AR debit = %d, want 20400", byAccount["ar"])
	}
	// rev-a net grouped (10000 + 2000), credited (negative net-debit).
	if byAccount["rev-a"] != -12000 {
		t.Fatalf("rev-a = %d, want -12000", byAccount["rev-a"])
	}
	if byAccount["rev-b"] != -5000 {
		t.Fatalf("rev-b = %d, want -5000", byAccount["rev-b"])
	}
	if byAccount["taxpay"] != -3400 {
		t.Fatalf("tax payable = %d, want -3400", byAccount["taxpay"])
	}
	if totals.NetMinor != 17000 || totals.TaxMinor != 3400 || totals.GrossMinor != 20400 {
		t.Fatalf("totals = %+v, want net=17000 tax=3400 gross=20400", totals)
	}
	if cmd.CommandID != "cmd-x" || cmd.Currency != "EUR" || cmd.Period != "2026-01" {
		t.Fatalf("cmd metadata = %+v", cmd)
	}
}

// A tax-free invoice omits the tax leg but still balances (>= 2 lines).
func TestBuildInvoiceJournalNoTax(t *testing.T) {
	inv := Invoice{Currency: "EUR", ARAccount: "ar", TaxAccount: "taxpay",
		Lines: []Line{{RevenueAccount: "rev", Quantity: 1, UnitPriceMinor: 10000}}}
	taxLines := []tax.LineResult{
		{Net: mustMoney(t, 10000, "EUR"), Tax: mustMoney(t, 0, "EUR"), Gross: mustMoney(t, 10000, "EUR")},
	}
	cmd, totals, err := buildInvoiceJournal(inv, taxLines, "2026-01", "cmd-y")
	if err != nil {
		t.Fatalf("buildInvoiceJournal: %v", err)
	}
	if len(cmd.Lines) != 2 { // AR + one revenue; no tax leg
		t.Fatalf("expected 2 lines (no tax leg), got %d: %+v", len(cmd.Lines), cmd.Lines)
	}
	if totals.TaxMinor != 0 || totals.GrossMinor != 10000 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestBuildInvoiceJournalLineMismatch(t *testing.T) {
	inv := Invoice{Currency: "EUR", ARAccount: "ar", TaxAccount: "t",
		Lines: []Line{{RevenueAccount: "rev", Quantity: 1, UnitPriceMinor: 100}}}
	if _, _, err := buildInvoiceJournal(inv, nil, "2026-01", "c"); err == nil {
		t.Fatal("expected error when tax result line count != invoice line count")
	}
}

func TestValidateDraft(t *testing.T) {
	good := DraftInput{
		ContactID: "c1", Currency: "EUR", IssueDate: "2026-01-15",
		ARAccount: "ar", TaxAccount: "tax",
		Lines: []Line{{RevenueAccount: "rev", Quantity: 1, UnitPriceMinor: 100, TaxJurisdiction: "DE", TaxCategory: "standard"}},
	}
	if err := validateDraft(good); err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}
	bad := func(mut func(*DraftInput)) DraftInput {
		d := good
		d.Lines = append([]Line(nil), good.Lines...)
		mut(&d)
		return d
	}
	cases := map[string]DraftInput{
		"no contact":  bad(func(d *DraftInput) { d.ContactID = "" }),
		"bad ccy":     bad(func(d *DraftInput) { d.Currency = "XYZ" }),
		"no date":     bad(func(d *DraftInput) { d.IssueDate = "" }),
		"no ar":       bad(func(d *DraftInput) { d.ARAccount = "" }),
		"no lines":    bad(func(d *DraftInput) { d.Lines = nil }),
		"no rev acct": bad(func(d *DraftInput) { d.Lines[0].RevenueAccount = "" }),
		"neg qty":     bad(func(d *DraftInput) { d.Lines[0].Quantity = -1 }),
		"no juris":    bad(func(d *DraftInput) { d.Lines[0].TaxJurisdiction = "" }),
	}
	for name, in := range cases {
		if err := validateDraft(in); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestFormatInvoiceNumber(t *testing.T) {
	for seq, want := range map[int64]string{1: "INV-000001", 42: "INV-000042", 1000000: "INV-1000000"} {
		if got := formatInvoiceNumber(seq); got != want {
			t.Errorf("formatInvoiceNumber(%d) = %q, want %q", seq, got, want)
		}
	}
}

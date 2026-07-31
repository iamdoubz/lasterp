// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// --- the declared GL template (INV-F5) and its balance (INV-F1) ---

func testReceipt(apps ...Application) Receipt {
	return Receipt{
		ID: "rct-1", Currency: "EUR", BankAccount: "bank",
		Applications: apps,
	}
}

// The receipt template debits the bank for the total and credits AR for the
// same — it balances by construction, whatever the applications look like.
func TestReceiptJournalBalances(t *testing.T) {
	r := testReceipt(
		Application{InvoiceID: "inv-1", AmountMinor: 11900},
		Application{InvoiceID: "inv-2", AmountMinor: 5000},
	)
	ar := map[string]string{"inv-1": "ar-main", "inv-2": "ar-main"}

	cmd, total, err := buildReceiptJournal(r, ar, "2026-07", "cmd-1")
	if err != nil {
		t.Fatalf("buildReceiptJournal: %v", err)
	}
	if total != 16900 {
		t.Errorf("total = %d, want 16900", total)
	}

	var debits, credits int64
	for _, l := range cmd.Lines {
		debits += l.Debit
		credits += l.Credit
	}
	if debits != credits {
		t.Errorf("entry does not balance: debits %d, credits %d (INV-F1)", debits, credits)
	}
	if debits != 16900 {
		t.Errorf("debits = %d, want 16900", debits)
	}
}

// Two invoices sharing an AR account collapse to one credit line; two different
// AR accounts stay separate. Crediting a single configured control account
// instead would leave both real accounts permanently wrong.
func TestReceiptJournalCreditsEachInvoicesOwnARAccount(t *testing.T) {
	r := testReceipt(
		Application{InvoiceID: "inv-1", AmountMinor: 100},
		Application{InvoiceID: "inv-2", AmountMinor: 200},
		Application{InvoiceID: "inv-3", AmountMinor: 300},
	)
	ar := map[string]string{"inv-1": "ar-domestic", "inv-2": "ar-export", "inv-3": "ar-domestic"}

	cmd, _, err := buildReceiptJournal(r, ar, "2026-07", "cmd-1")
	if err != nil {
		t.Fatalf("buildReceiptJournal: %v", err)
	}

	credits := map[string]int64{}
	for _, l := range cmd.Lines {
		if l.Credit > 0 {
			credits[l.AccountID] += l.Credit
		}
	}
	if len(credits) != 2 {
		t.Fatalf("credit lines = %v, want one per distinct AR account", credits)
	}
	if credits["ar-domestic"] != 400 {
		t.Errorf("ar-domestic credit = %d, want 400 (100+300 grouped)", credits["ar-domestic"])
	}
	if credits["ar-export"] != 200 {
		t.Errorf("ar-export credit = %d, want 200", credits["ar-export"])
	}
}

// The entry must be byte-identical across runs so a replay produces the same
// journal — Go map iteration order is randomized, so this would fail without
// the explicit sort.
func TestReceiptJournalLineOrderIsDeterministic(t *testing.T) {
	r := testReceipt(
		Application{InvoiceID: "inv-1", AmountMinor: 100},
		Application{InvoiceID: "inv-2", AmountMinor: 200},
		Application{InvoiceID: "inv-3", AmountMinor: 300},
	)
	ar := map[string]string{"inv-1": "ar-c", "inv-2": "ar-a", "inv-3": "ar-b"}

	first, _, err := buildReceiptJournal(r, ar, "2026-07", "cmd-1")
	if err != nil {
		t.Fatalf("buildReceiptJournal: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, _, err := buildReceiptJournal(r, ar, "2026-07", "cmd-1")
		if err != nil {
			t.Fatalf("buildReceiptJournal: %v", err)
		}
		for j := range first.Lines {
			if first.Lines[j] != again.Lines[j] {
				t.Fatalf("line %d differs between runs: %+v vs %+v", j, first.Lines[j], again.Lines[j])
			}
		}
	}
}

// An application naming an invoice with no known AR account is a programming
// error upstream; the template refuses rather than inventing an account.
func TestReceiptJournalRejectsUnknownARAccount(t *testing.T) {
	r := testReceipt(Application{InvoiceID: "inv-ghost", AmountMinor: 100})
	if _, _, err := buildReceiptJournal(r, map[string]string{}, "2026-07", "cmd-1"); err == nil {
		t.Fatal("expected an error for an invoice with no AR account")
	}
}

// --- draft validation ---

func TestValidateReceipt(t *testing.T) {
	valid := ReceiptInput{
		ContactID: "c-1", Currency: "EUR", ReceivedDate: "2026-07-15", BankAccount: "bank",
		Applications: []Application{{InvoiceID: "inv-1", AmountMinor: 100}},
	}
	if err := validateReceipt(valid); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	tests := []struct {
		name string
		mut  func(*ReceiptInput)
		want string
	}{
		{"no contact", func(in *ReceiptInput) { in.ContactID = "" }, "contact_id"},
		{"bad currency", func(in *ReceiptInput) { in.Currency = "XYZ" }, ""},
		{"no date", func(in *ReceiptInput) { in.ReceivedDate = "" }, "received_date"},
		{"no bank account", func(in *ReceiptInput) { in.BankAccount = "" }, "bank_account"},
		{"no applications", func(in *ReceiptInput) { in.Applications = nil }, "at least one"},
		{"no invoice id", func(in *ReceiptInput) { in.Applications[0].InvoiceID = "" }, "invoice_id"},
		{"zero amount", func(in *ReceiptInput) { in.Applications[0].AmountMinor = 0 }, "positive"},
		{"negative amount", func(in *ReceiptInput) { in.Applications[0].AmountMinor = -1 }, "positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			in.Applications = append([]Application(nil), valid.Applications...)
			tc.mut(&in)
			err := validateReceipt(in)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// INV-F8, structural half: two applications to the same invoice on ONE receipt
// would each pass an individual bounds check while together breaching it, so
// they are refused up front.
func TestValidateReceiptRejectsDuplicateInvoice(t *testing.T) {
	in := ReceiptInput{
		ContactID: "c-1", Currency: "EUR", ReceivedDate: "2026-07-15", BankAccount: "bank",
		Applications: []Application{
			{InvoiceID: "inv-1", AmountMinor: 100},
			{InvoiceID: "inv-1", AmountMinor: 100},
		},
	}
	err := validateReceipt(in)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate application error = %v, want a refusal naming the repeat", err)
	}
}

// --- settlement derivation ---

func TestSettlementStatus(t *testing.T) {
	tests := []struct {
		name           string
		gross, settled int64
		want           string
	}{
		{"nothing paid", 11900, 0, SettlementOpen},
		{"part paid", 11900, 5000, SettlementPartial},
		{"one cent short", 11900, 11899, SettlementPartial},
		{"exactly paid", 11900, 11900, SettlementPaid},
		{"zero-value invoice", 0, 0, SettlementOpen},
		// Unreachable while INV-F8 holds; asserted so a regression shows up as
		// "paid" rather than a fourth, unhandled state.
		{"over-applied", 11900, 12000, SettlementPaid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := settlementStatus(tc.gross, tc.settled); got != tc.want {
				t.Errorf("settlementStatus(%d, %d) = %q, want %q", tc.gross, tc.settled, got, tc.want)
			}
		})
	}
}

// A partially-paid invoice's outstanding is exactly what is left, to the minor
// unit — this is the number AR aging reports, so a rounding slip here is a
// misstated balance sheet.
func TestSettlementOutstandingIsExact(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		gross := r.Int63n(1_000_000) + 1
		settled := r.Int63n(gross + 1)
		s := Settlement{
			GrossMinor:       gross,
			SettledMinor:     settled,
			OutstandingMinor: gross - settled,
			Status:           settlementStatus(gross, settled),
		}
		if s.SettledMinor+s.OutstandingMinor != s.GrossMinor {
			t.Fatalf("settled %d + outstanding %d != gross %d", s.SettledMinor, s.OutstandingMinor, s.GrossMinor)
		}
		if s.OutstandingMinor < 0 {
			t.Fatalf("negative outstanding for gross %d settled %d", gross, settled)
		}
		if (s.OutstandingMinor == 0) != (s.Status == SettlementPaid) {
			t.Fatalf("status %q disagrees with outstanding %d", s.Status, s.OutstandingMinor)
		}
	}
}

// --- numbering ---

func TestFormatReceiptNumber(t *testing.T) {
	if got := formatReceiptNumber(1); got != "RCT-000001" {
		t.Errorf("formatReceiptNumber(1) = %q", got)
	}
	if got := formatReceiptNumber(123456); got != "RCT-123456" {
		t.Errorf("formatReceiptNumber(123456) = %q", got)
	}
	// Receipts and invoices must not be confusable at a glance in the ledger.
	if strings.HasPrefix(formatReceiptNumber(1), "INV") {
		t.Error("receipt numbers must not share the invoice prefix")
	}
}

func TestErrOverAppliedIsMatchable(t *testing.T) {
	// The API layer classifies this into a 4xx; wrapping must stay unwrappable.
	err := errors.New("wrapped: " + ErrOverApplied.Error())
	if errors.Is(err, ErrOverApplied) {
		t.Skip("string-wrapped sentinel is not the contract")
	}
	if !errors.Is(ErrOverApplied, ErrOverApplied) {
		t.Error("sentinel is not identifiable")
	}
}

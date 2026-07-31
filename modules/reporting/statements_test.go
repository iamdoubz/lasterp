// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"math/rand"
	"testing"

	"github.com/iamdoubz/lasterp/modules/ledger"
)

// The statements are pure functions of Data, so they test without a database.
// The database-backed reconciliation against the event-fold oracle (INV-E5) is
// in statements_integrity_test.go.

func acct(id, code, name, typ string) Account {
	return Account{ID: id, Code: code, Name: name, Type: typ}
}

// A tiny but complete set of books: an invoice posted (AR/revenue/tax) and part
// of it received into the bank.
func testData() Data {
	return Data{
		Currency: "EUR",
		Accounts: []Account{
			acct("a-bank", "1000", "Bank", ledger.AccountAsset),
			acct("a-ar", "1100", "Accounts receivable", ledger.AccountAsset),
			acct("a-tax", "2200", "Tax payable", ledger.AccountLiability),
			acct("a-cap", "3000", "Share capital", ledger.AccountEquity),
			acct("a-rev", "4000", "Revenue", ledger.AccountIncome),
			acct("a-exp", "5000", "Office costs", ledger.AccountExpense),
		},
		Balances: ledger.TrialBalance{
			// Invoice: DR AR 120000 / CR revenue 100000, CR tax 20000.
			// Receipt: DR bank 50000 / CR AR 50000.
			// Capital: DR bank 10000 / CR equity 10000. Expense: DR 3000 / CR bank 3000.
			"a-bank": {"EUR": 50000 + 10000 - 3000},
			"a-ar":   {"EUR": 120000 - 50000},
			"a-tax":  {"EUR": -20000},
			"a-cap":  {"EUR": -10000},
			"a-rev":  {"EUR": -100000},
			"a-exp":  {"EUR": 3000},
		},
	}
}

// Every entry balances (INV-F1), so the trial balance's debits equal its
// credits. The report does not assert this — it shows it, and this test is what
// fails if it stops being true.
func TestTrialBalanceDebitsEqualCredits(t *testing.T) {
	rep := TrialBalance(testData())

	var debits, credits int64
	for _, row := range rep.Totals {
		switch row.Key {
		case "debits":
			debits = row.AmountMinor
		case "credits":
			credits = row.AmountMinor
		}
	}
	if debits == 0 {
		t.Fatal("trial balance reported no debits")
	}
	if debits != credits {
		t.Errorf("Σdebits %d != Σcredits %d", debits, credits)
	}
}

// Accounts with no movement are noise on a trial balance.
func TestTrialBalanceOmitsZeroAccounts(t *testing.T) {
	d := testData()
	d.Accounts = append(d.Accounts, acct("a-unused", "9999", "Never used", ledger.AccountAsset))

	for _, row := range TrialBalance(d).Rows {
		if row.Key == "9999" {
			t.Error("an account with no movement appeared on the trial balance")
		}
	}
}

// Income reads positive when the business earned money, despite being stored as
// a negative net (Σdebits − Σcredits).
func TestProfitAndLossSignsAndTotals(t *testing.T) {
	rep := ProfitAndLoss(testData())

	totals := map[string]int64{}
	for _, row := range rep.Totals {
		totals[row.Key] = row.AmountMinor
	}
	if totals["income"] != 100000 {
		t.Errorf("income = %d, want 100000 (positive when earned)", totals["income"])
	}
	if totals["expenses"] != 3000 {
		t.Errorf("expenses = %d, want 3000", totals["expenses"])
	}
	if totals["net_income"] != 97000 {
		t.Errorf("net income = %d, want 97000", totals["net_income"])
	}
	if got := NetIncome(testData()); got != totals["net_income"] {
		t.Errorf("NetIncome() = %d but the P&L reports %d — they must be one number", got, totals["net_income"])
	}
}

// The P&L carries only income and expense accounts.
func TestProfitAndLossExcludesBalanceSheetAccounts(t *testing.T) {
	for _, row := range ProfitAndLoss(testData()).Rows {
		if got := row.SourceIDs[0]; got == "a-bank" || got == "a-ar" || got == "a-tax" || got == "a-cap" {
			t.Errorf("balance-sheet account %s appeared on the P&L", got)
		}
	}
}

// The statement's own reconciliation: assets = liabilities + equity, with
// current-period net income folded into equity as unclosed retained earnings.
// Omitting net income is exactly what makes a naive balance sheet fail.
func TestBalanceSheetBalances(t *testing.T) {
	rep := BalanceSheet(testData())

	totals := map[string]int64{}
	for _, row := range rep.Totals {
		totals[row.Key] = row.AmountMinor
	}
	assets := totals["assets"]
	liabilities := totals["liabilities"]
	equity := totals["equity"]

	if assets == 0 {
		t.Fatal("balance sheet reported no assets")
	}
	if assets != liabilities+equity {
		t.Errorf("assets %d != liabilities %d + equity %d (difference %d)",
			assets, liabilities, equity, assets-liabilities-equity)
	}
	// Equity must include the period's earnings, not just contributed capital.
	if equity != 10000+97000 {
		t.Errorf("equity = %d, want 107000 (10000 capital + 97000 net income)", equity)
	}
}

// A misconfigured account type must be VISIBLE, not silently dropped: dropping
// it would leave a statement that still looked balanced while being wrong.
func TestBalanceSheetSurfacesUnclassifiedAccounts(t *testing.T) {
	d := testData()
	// "revenue" is not in the ledger's closed set {asset, liability, equity,
	// income, expense} — and generic CRUD accepts it (decisions §5).
	d.Accounts = append(d.Accounts, acct("a-bogus", "4999", "Misfiled", "revenue"))
	d.Balances["a-bogus"] = map[string]int64{"EUR": -7000}

	if got := Unclassified(d); got != -7000 {
		t.Errorf("Unclassified() = %d, want -7000", got)
	}

	var found bool
	for _, row := range BalanceSheet(d).Totals {
		if row.Key == TypeUnclassified {
			found = true
			if row.AmountMinor != -7000 {
				t.Errorf("unclassified total = %d, want -7000", row.AmountMinor)
			}
		}
	}
	if !found {
		t.Error("an account with an unrecognized type vanished from the balance sheet without a trace")
	}
}

// A well-formed tenant shows no unclassified line at all, so the bucket cannot
// quietly become normal furniture.
func TestBalanceSheetHidesUnclassifiedWhenEmpty(t *testing.T) {
	for _, row := range BalanceSheet(testData()).Totals {
		if row.Key == TypeUnclassified {
			t.Error("unclassified total rendered for a well-formed chart of accounts")
		}
	}
}

// An unclassified account must not be silently absorbed into a real section.
func TestUnclassifiedIsNotCountedAsAnything(t *testing.T) {
	clean := BalanceSheet(testData())
	d := testData()
	d.Accounts = append(d.Accounts, acct("a-bogus", "4999", "Misfiled", "not-a-type"))
	d.Balances["a-bogus"] = map[string]int64{"EUR": 5000}
	dirty := BalanceSheet(d)

	total := func(rep Report, key string) int64 {
		for _, row := range rep.Totals {
			if row.Key == key {
				return row.AmountMinor
			}
		}
		return 0
	}
	for _, key := range []string{"assets", "liabilities", "equity"} {
		if total(clean, key) != total(dirty, key) {
			t.Errorf("%s changed from %d to %d when an unclassified account was added",
				key, total(clean, key), total(dirty, key))
		}
	}
}

// classify is total: anything outside the closed set lands in one visible place.
func TestClassifyIsTotal(t *testing.T) {
	for _, known := range []string{
		ledger.AccountAsset, ledger.AccountLiability, ledger.AccountEquity,
		ledger.AccountIncome, ledger.AccountExpense,
	} {
		if classify(known) != known {
			t.Errorf("classify(%q) = %q, want it unchanged", known, classify(known))
		}
	}
	for _, unknown := range []string{"", "revenue", "Asset", "ASSET", "liabilities", "🙂"} {
		if got := classify(unknown); got != TypeUnclassified {
			t.Errorf("classify(%q) = %q, want %q", unknown, got, TypeUnclassified)
		}
	}
}

// Presentation sign is an involution against the raw net, whatever the value.
func TestPresentationSignProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	types := []string{
		ledger.AccountAsset, ledger.AccountLiability, ledger.AccountEquity,
		ledger.AccountIncome, ledger.AccountExpense,
	}
	for i := 0; i < 500; i++ {
		typ := types[rng.Intn(len(types))]
		net := rng.Int63n(2_000_000) - 1_000_000
		p := presentation(typ, net)
		if presentation(typ, p*normalBalance(typ)) != p {
			t.Fatalf("presentation is not sign-stable for %s at %d", typ, net)
		}
		switch typ {
		case ledger.AccountAsset, ledger.AccountExpense:
			if p != net {
				t.Fatalf("%s should present its raw net: got %d want %d", typ, p, net)
			}
		default:
			if p != -net {
				t.Fatalf("%s should present its negated net: got %d want %d", typ, p, -net)
			}
		}
	}
}

// Rows are sorted, so a report is diff-friendly and an export is reproducible.
func TestReportRowsAreSorted(t *testing.T) {
	for _, rep := range []Report{TrialBalance(testData()), ProfitAndLoss(testData())} {
		for i := 1; i < len(rep.Rows); i++ {
			if rep.Rows[i-1].Key > rep.Rows[i].Key {
				t.Errorf("%s rows are unsorted at %d: %q then %q",
					rep.Name, i, rep.Rows[i-1].Key, rep.Rows[i].Key)
			}
		}
	}
}

// Every statement row carries the account it came from, which is what makes
// drill-down possible (docs/21 §2).
func TestStatementRowsCarrySourceIDs(t *testing.T) {
	for _, rep := range []Report{TrialBalance(testData()), ProfitAndLoss(testData())} {
		for _, row := range rep.Rows {
			if len(row.SourceIDs) == 0 {
				t.Errorf("%s row %q carries no source id — nothing to drill into", rep.Name, row.Key)
			}
		}
	}
}

// Aging bands are contiguous and total: every age lands in exactly one bucket.
func TestAgingBucketsAreTotal(t *testing.T) {
	tests := []struct {
		days int
		want string
	}{
		{0, "current"}, {1, "1_30"}, {30, "1_30"}, {31, "31_60"}, {60, "31_60"},
		{61, "61_90"}, {90, "61_90"}, {91, "90_plus"}, {5000, "90_plus"},
	}
	for _, tc := range tests {
		if got := bucketFor(tc.days); got != tc.want {
			t.Errorf("bucketFor(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

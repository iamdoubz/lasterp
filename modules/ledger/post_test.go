package ledger

import (
	"errors"
	"testing"
)

// validateEntry is the pure INV-F1 / structural gate; test it without a DB.
func TestValidateEntry(t *testing.T) {
	base := func(lines []Line) PostCmd {
		return PostCmd{Period: "2026-01", Currency: "USD", CommandID: "c1", Lines: lines}
	}
	balanced := []Line{{AccountID: "a", Debit: 1000}, {AccountID: "b", Credit: 1000}}

	cases := []struct {
		name string
		cmd  PostCmd
		want error
	}{
		{"balanced", base(balanced), nil},
		{"unbalanced", base([]Line{{AccountID: "a", Debit: 1000}, {AccountID: "b", Credit: 999}}), ErrUnbalanced},
		{"too few lines", base([]Line{{AccountID: "a", Debit: 1000}}), ErrTooFewLines},
		{"line both debit and credit", base([]Line{{AccountID: "a", Debit: 500, Credit: 500}, {AccountID: "b", Credit: 1000}}), ErrLineNotXOR},
		{"line neither", base([]Line{{AccountID: "a"}, {AccountID: "b", Credit: 1000}}), ErrLineNotXOR},
		{"negative", base([]Line{{AccountID: "a", Debit: -1000}, {AccountID: "b", Credit: -1000}}), ErrNegativeAmount},
		{"multi-line balanced", base([]Line{{AccountID: "a", Debit: 700}, {AccountID: "b", Debit: 300}, {AccountID: "c", Credit: 1000}}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateEntry(c.cmd)
			if !errors.Is(err, c.want) {
				t.Fatalf("validateEntry = %v, want %v", err, c.want)
			}
		})
	}
}

func TestValidateEntryRejectsBadCurrency(t *testing.T) {
	cmd := PostCmd{Period: "2026-01", Currency: "ZZZ", CommandID: "c1",
		Lines: []Line{{AccountID: "a", Debit: 1}, {AccountID: "b", Credit: 1}}}
	if err := validateEntry(cmd); err == nil {
		t.Fatal("want error for unknown currency ZZZ")
	}
}

func TestValidateEntryRequiresCommandAndPeriod(t *testing.T) {
	l := []Line{{AccountID: "a", Debit: 1}, {AccountID: "b", Credit: 1}}
	if err := validateEntry(PostCmd{Currency: "USD", Period: "2026-01", Lines: l}); err == nil {
		t.Fatal("want error for missing command id")
	}
	if err := validateEntry(PostCmd{Currency: "USD", CommandID: "c", Lines: l}); err == nil {
		t.Fatal("want error for missing period")
	}
}

func TestValidAccountTypes(t *testing.T) {
	for _, ty := range []string{AccountAsset, AccountLiability, AccountEquity, AccountIncome, AccountExpense} {
		if !validAccountTypes[ty] {
			t.Fatalf("%q should be a valid account type", ty)
		}
	}
	if validAccountTypes["nonsense"] {
		t.Fatal("nonsense should not be a valid account type")
	}
}

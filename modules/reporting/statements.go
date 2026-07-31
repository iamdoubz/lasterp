// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// The three ledger statements. Each is a PURE function of Data — no context, no
// database, no clock — which is what lets the test suite compute every one of
// them twice (once from the projection, once from a fold of the raw event log)
// and assert equality. That equality is INV-E5, and it is this WP's acceptance
// criterion.

// TrialBalance lists every account's net movement with debit/credit
// presentation columns. Because every entry balances (INV-F1), total debits
// equal total credits — the report asserts nothing, it simply shows it, and the
// test suite is what fails if it stops being true.
func TrialBalance(d Data) Report {
	rep := Report{
		Name:     "trial_balance",
		Title:    "Trial balance",
		Currency: d.Currency,
		Columns:  []string{"Account", "Debit", "Credit"},
	}
	var totalDebit, totalCredit int64
	for _, a := range d.Accounts {
		net := d.netFor(a.ID)
		if net == 0 {
			continue // an account with no movement is noise on a trial balance
		}
		debit, credit := int64(0), int64(0)
		if net > 0 {
			debit = net
		} else {
			credit = -net
		}
		totalDebit += debit
		totalCredit += credit
		rep.Rows = append(rep.Rows, Row{
			Label: a.Code + " " + a.Name, Key: a.Code, Currency: d.Currency,
			AmountMinor: net, SourceIDs: []string{a.ID},
		})
	}
	sortRows(rep.Rows)
	rep.Totals = []Row{
		{Label: "Total debits", Key: "debits", Currency: d.Currency, AmountMinor: totalDebit},
		{Label: "Total credits", Key: "credits", Currency: d.Currency, AmountMinor: totalCredit},
	}
	return rep
}

// ProfitAndLoss reports income and expense accounts, and the net income they
// produce. Amounts are sign-normalized to their normal balance, so income reads
// positive when the business earned money.
func ProfitAndLoss(d Data) Report {
	rep := Report{
		Name:     "profit_and_loss",
		Title:    "Profit and loss",
		Currency: d.Currency,
		Columns:  []string{"Account", "Amount"},
	}
	var income, expense int64
	for _, a := range d.Accounts {
		section := classify(a.Type)
		if section != ledger.AccountIncome && section != ledger.AccountExpense {
			continue
		}
		net := d.netFor(a.ID)
		if net == 0 {
			continue
		}
		amount := presentation(a.Type, net)
		if section == ledger.AccountIncome {
			income += amount
		} else {
			expense += amount
		}
		rep.Rows = append(rep.Rows, Row{
			Label: a.Code + " " + a.Name, Key: section + ":" + a.Code, Currency: d.Currency,
			AmountMinor: amount, SourceIDs: []string{a.ID},
		})
	}
	sortRows(rep.Rows)
	rep.Totals = []Row{
		{Label: "Total income", Key: "income", Currency: d.Currency, AmountMinor: income},
		{Label: "Total expenses", Key: "expenses", Currency: d.Currency, AmountMinor: expense},
		{Label: "Net income", Key: "net_income", Currency: d.Currency, AmountMinor: income - expense},
	}
	return rep
}

// NetIncome is the P&L bottom line, exposed separately because the balance
// sheet needs it: income earned this period is equity the ledger has not yet
// closed into retained earnings, and omitting it is what makes a naive balance
// sheet fail to balance.
func NetIncome(d Data) int64 {
	var income, expense int64
	for _, a := range d.Accounts {
		switch classify(a.Type) {
		case ledger.AccountIncome:
			income += presentation(a.Type, d.netFor(a.ID))
		case ledger.AccountExpense:
			expense += presentation(a.Type, d.netFor(a.ID))
		}
	}
	return income - expense
}

// BalanceSheet reports assets, liabilities and equity, with current-period net
// income folded into equity as unclosed retained earnings.
//
// The identity assets = liabilities + equity holds because every journal entry
// balances (INV-F1) and every account is classified into exactly one section —
// including the unclassified bucket, which is reported rather than dropped
// precisely so that a misconfigured chart of accounts cannot make the statement
// silently wrong (WP-1.6-decisions.md §5).
func BalanceSheet(d Data) Report {
	rep := Report{
		Name:     "balance_sheet",
		Title:    "Balance sheet",
		Currency: d.Currency,
		Columns:  []string{"Account", "Amount"},
	}
	var assets, liabilities, equity, unclassified int64
	for _, a := range d.Accounts {
		section := classify(a.Type)
		if section == ledger.AccountIncome || section == ledger.AccountExpense {
			continue // they roll up into net income below
		}
		net := d.netFor(a.ID)
		if net == 0 {
			continue
		}
		amount := presentation(a.Type, net)
		switch section {
		case ledger.AccountAsset:
			assets += amount
		case ledger.AccountLiability:
			liabilities += amount
		case ledger.AccountEquity:
			equity += amount
		default:
			// Unclassified accounts are presented raw (Σdebits − Σcredits):
			// without a known normal balance there is no meaningful sign to
			// normalize to, and inventing one would hide the problem.
			unclassified += net
		}
		rep.Rows = append(rep.Rows, Row{
			Label: a.Code + " " + a.Name, Key: section + ":" + a.Code, Currency: d.Currency,
			AmountMinor: amount, SourceIDs: []string{a.ID},
		})
	}
	sortRows(rep.Rows)

	// Current-period earnings are equity the ledger has not closed into retained
	// earnings yet; omitting them is what makes a naive balance sheet fail to
	// balance. Suppressed when zero, like every other zero row — a tenant with
	// no activity should produce an empty statement, not one phantom line.
	retained := NetIncome(d)
	if retained != 0 {
		rep.Rows = append(rep.Rows, Row{
			Label: "Current period net income", Key: "equity:~retained", Currency: d.Currency,
			AmountMinor: retained,
		})
	}
	equity += retained

	rep.Totals = []Row{
		{Label: "Total assets", Key: "assets", Currency: d.Currency, AmountMinor: assets},
		{Label: "Total liabilities", Key: "liabilities", Currency: d.Currency, AmountMinor: liabilities},
		{Label: "Total equity", Key: "equity", Currency: d.Currency, AmountMinor: equity},
	}
	if unclassified != 0 {
		// Only shown when non-zero, so a healthy tenant's statement is clean and
		// an unhealthy one cannot be overlooked.
		rep.Totals = append(rep.Totals, Row{
			Label: "Unclassified (accounts with an unrecognized type)", Key: TypeUnclassified,
			Currency: d.Currency, AmountMinor: unclassified,
		})
	}
	return rep
}

// Unclassified totals the accounts whose type is outside the ledger's closed
// set. It is exported so callers (and the test suite) can assert it is zero for
// a well-formed tenant rather than discovering it in a rendered statement.
func Unclassified(d Data) int64 {
	var total int64
	for _, a := range d.Accounts {
		if classify(a.Type) == TypeUnclassified {
			total += d.netFor(a.ID)
		}
	}
	return total
}

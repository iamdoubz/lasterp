// SPDX-License-Identifier: AGPL-3.0-only

// Package reporting is the WP-1.6 canned financial reports and metrics layer
// (docs/21). Everything here is READ-ONLY by construction: the module owns no
// write path, no table, and no lifecycle — which is the strongest available form
// of the permission-leak guarantee, because there is nothing to leak *into*.
//
// Every financial report is a pure function of (trial balance, chart of
// accounts). The trial balance is a projection of the ledger's append-only
// event log, so a report is transitively a pure function of the log (INV-E5) —
// and the test suite proves it by computing each report twice: once from the
// materialized projection, once from a fold of the raw events.
//
// Per the WP-1.4 boundary precedent this module imports the siblings it declares
// in `requires:` — modules/ledger and modules/invoicing. No cycle: neither
// imports reporting.
package reporting

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// Account is the chart-of-accounts row a report needs: how to label a line and
// which statement it belongs to.
type Account struct {
	ID       string
	Code     string
	Name     string
	Type     string
	Currency string
}

// Row is one line of a report. Values are integer minor units (INV-F4) and
// SourceIDs carries the ids the row was computed from, which is what makes
// drill-down possible without a second query engine (docs/21 §2).
type Row struct {
	Label       string   `json:"label"`
	Key         string   `json:"key"`
	Currency    string   `json:"currency"`
	AmountMinor int64    `json:"amount_minor"`
	SourceIDs   []string `json:"source_ids,omitempty"`
}

// Report is a rendered report: named columns, rows, and totals. It is the one
// shape every canned report produces, so export (CSV/XLSX) and the API surface
// are written once rather than per report.
type Report struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	Currency string   `json:"currency"`
	Columns  []string `json:"columns"`
	Rows     []Row    `json:"rows"`
	Totals   []Row    `json:"totals"`
	// Basis records how the report was computed when that is not obvious and
	// getting it wrong would mislead — e.g. AR aging ages off issue date, not
	// due date (WP-1.6-decisions.md §8).
	Basis string `json:"basis,omitempty"`
}

// TypeUnclassified is where accounts outside the ledger's closed type set land.
//
// It exists because they CAN exist: ledger.CreateAccount validates the type, but
// the generic CRUD route does not (kernel/metadata has no enum options —
// WP-1.6-decisions.md §5). An account with an unrecognized type would otherwise
// silently vanish from both the P&L and the balance sheet, and both statements
// would still *look* balanced while being wrong. A visible bucket turns silent
// misstatement into an obvious question.
const TypeUnclassified = "unclassified"

// classify maps an account type to its statement section, funnelling anything
// outside the closed set into TypeUnclassified rather than dropping it.
func classify(accountType string) string {
	switch accountType {
	case ledger.AccountAsset, ledger.AccountLiability, ledger.AccountEquity,
		ledger.AccountIncome, ledger.AccountExpense:
		return accountType
	default:
		return TypeUnclassified
	}
}

// normalBalance reports whether an account type is debit-normal. Assets and
// expenses increase with debits; liabilities, equity and income increase with
// credits. The trial balance stores Σdebits − Σcredits, so credit-normal
// balances arrive negative and are flipped for presentation.
func normalBalance(accountType string) int64 {
	switch classify(accountType) {
	case ledger.AccountAsset, ledger.AccountExpense:
		return 1
	default:
		return -1
	}
}

// presentation converts a raw net (Σdebits − Σcredits) into the sign a reader
// expects: a positive number means "more of what this account normally holds".
func presentation(accountType string, net int64) int64 {
	return net * normalBalance(accountType)
}

// Data is everything the pure report functions need. Building it is the only
// part that touches the database.
type Data struct {
	Accounts []Account
	Balances ledger.TrialBalance
	Currency string
}

// netFor returns an account's net movement in the report currency.
func (d Data) netFor(accountID string) int64 {
	byCurrency := d.Balances[accountID]
	if byCurrency == nil {
		return 0
	}
	return byCurrency[d.Currency]
}

// Load assembles report Data for tenant: it catches the projection up to the
// log, then reads the balances and the chart of accounts.
//
// The catch-up is what makes reports correct rather than merely fast — before
// WP-1.6 nothing advanced the projection at all, so it was empty in a running
// system (WP-1.6-decisions.md §4a).
func Load(ctx context.Context, db *storage.DB, tenant tenancy.ID, currency string) (Data, error) {
	if err := ledger.EnsureBalances(ctx, db, tenant); err != nil {
		return Data{}, fmt.Errorf("reporting: refresh balances: %w", err)
	}
	balances, err := ledger.ReadTrialBalance(ctx, db, tenant)
	if err != nil {
		return Data{}, fmt.Errorf("reporting: read balances: %w", err)
	}
	accounts, err := loadAccounts(ctx, db, tenant)
	if err != nil {
		return Data{}, err
	}
	return Data{Accounts: accounts, Balances: balances, Currency: currency}, nil
}

// authorizeRead is the single permission gate every report and metric passes
// through. docs/21 §1 is explicit that the query engine enforces this, not the
// UI: a report must never compute over rows its viewer could not read (INV-T1).
//
// The reads underneath are additionally tenant-scoped by RLS, so this is the
// second of two independent barriers, not the only one.
func authorizeRead(ctx context.Context, db *storage.DB, object string) error {
	_, err := authz.Authorize(ctx, db, object, "read")
	return err
}

// sortRows orders rows by key for a stable, diff-friendly report.
func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
}

// isPermissionDenied reports whether err is an authz refusal, so callers that
// legitimately skip inaccessible data (EvaluateAll) can distinguish "you may not
// see this" from "something broke".
func isPermissionDenied(err error) bool {
	return errors.Is(err, authz.ErrPermissionDenied) || errors.Is(err, authz.ErrNoActor)
}

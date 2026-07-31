// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// ErrUnknownReport is returned for a report name that is not canned.
var ErrUnknownReport = errors.New("reporting: unknown report")

// Definition is a canned report: its name, what it needs permission to read,
// and how to run it. Object+Action is checked before the report computes, so a
// report can never render rows its caller could not read directly (INV-T1,
// docs/21 §1 — enforced in the engine, not the UI).
type Definition struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Object string `json:"object"`
	Action string `json:"action"`

	run func(context.Context, *storage.DB, tenancy.ID, Scope) (Report, error)
}

// definitions is the canned catalog. Every financial statement runs off the same
// Data, so they all declare the same permission: being able to read journal
// entries is what it means to be allowed to see the books.
var definitions = map[string]Definition{}

func init() {
	statement := func(name, title string, build func(Data) Report) Definition {
		return Definition{
			Name: name, Title: title,
			Object: ledger.ObjectJournalEntry, Action: "read",
			run: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, p Scope) (Report, error) {
				d, err := Load(ctx, db, tenant, p.Currency)
				if err != nil {
					return Report{}, err
				}
				return build(d), nil
			},
		}
	}
	for _, d := range []Definition{
		statement("trial_balance", "Trial balance", TrialBalance),
		statement("profit_and_loss", "Profit and loss", ProfitAndLoss),
		statement("balance_sheet", "Balance sheet", BalanceSheet),
		{
			Name: "ar_aging", Title: "AR aging",
			Object: invoicing.ObjectInvoice, Action: "read",
			run: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, p Scope) (Report, error) {
				return ARAging(ctx, db, tenant, p.Currency, p.AsOf)
			},
		},
	} {
		definitions[d.Name] = d
	}
}

// Reports lists the canned catalog, sorted.
func Reports() []Definition {
	out := make([]Definition, 0, len(definitions))
	for _, d := range definitions {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run executes a canned report after checking the permission it declares.
//
// The gate is here, at the single entry point every caller goes through, rather
// than inside each report body — one place to audit, and a new report cannot
// forget it.
func Run(ctx context.Context, db *storage.DB, tenant tenancy.ID, name string, p Scope) (Report, error) {
	d, ok := definitions[name]
	if !ok {
		return Report{}, fmt.Errorf("%w: %q", ErrUnknownReport, name)
	}
	if p.Currency == "" {
		return Report{}, errors.New("reporting: currency is required")
	}
	if p.AsOf.IsZero() {
		p.AsOf = time.Now().UTC()
	}
	if err := authorizeRead(ctx, db, d.Object); err != nil {
		return Report{}, err
	}
	return d.run(ctx, db, tenant, p)
}

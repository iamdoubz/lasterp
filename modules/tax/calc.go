// SPDX-License-Identifier: AGPL-3.0-only

package tax

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Line is one taxable line of a document, with its rate already resolved.
type Line struct {
	Jurisdiction string
	Category     string
	// Base is the taxable amount. When Inclusive is false (the default,
	// exclusive) it is the net and tax is added on top; when Inclusive is true
	// it is the gross and the tax is extracted out of it.
	Base      money.Money
	Rate      *big.Rat
	Rounding  money.RoundingMode
	Inclusive bool
}

// LineResult is the computed tax split for one line: Net + Tax = Gross, always
// (the cent is conserved even for inclusive extraction — INV-F4).
type LineResult struct {
	Net   money.Money
	Tax   money.Money
	Gross money.Money
}

// TaxTotal is the summed net and tax for one (jurisdiction, category) group.
type TaxTotal struct {
	Jurisdiction string
	Category     string
	Net          money.Money
	Tax          money.Money
}

// Result is the full tax breakdown of a document: one LineResult per input line
// (same order) plus per-(jurisdiction, category) totals, sorted for
// determinism.
type Result struct {
	Lines  []LineResult
	Totals []TaxTotal
}

// Document is a set of taxable lines to compute tax for.
type Document struct {
	Lines []Line
}

// Calculate is the pure (no-DB) document tax calculation — the golden-file
// target and what invoicing calls after resolving rates. It rounds per line
// (v1) and groups totals by (jurisdiction, category). All arithmetic goes
// through kernel/money (INV-F4).
func Calculate(doc Document) (Result, error) {
	res := Result{Lines: make([]LineResult, 0, len(doc.Lines))}
	// Accumulate group totals keyed by "jurisdiction\x00category", preserving a
	// stable Money currency per group.
	type key struct{ j, c string }
	groups := map[key]*TaxTotal{}

	for i, ln := range doc.Lines {
		lr, err := calcLine(ln)
		if err != nil {
			return Result{}, fmt.Errorf("tax: line %d: %w", i, err)
		}
		res.Lines = append(res.Lines, lr)

		k := key{ln.Jurisdiction, ln.Category}
		t, ok := groups[k]
		if !ok {
			zeroNet, _ := money.Zero(lr.Net.Currency())
			zeroTax, _ := money.Zero(lr.Tax.Currency())
			t = &TaxTotal{Jurisdiction: ln.Jurisdiction, Category: ln.Category, Net: zeroNet, Tax: zeroTax}
			groups[k] = t
		}
		net, err := t.Net.Add(lr.Net)
		if err != nil {
			return Result{}, fmt.Errorf("tax: line %d: %w", i, err)
		}
		tax, err := t.Tax.Add(lr.Tax)
		if err != nil {
			return Result{}, fmt.Errorf("tax: line %d: %w", i, err)
		}
		t.Net, t.Tax = net, tax
	}

	res.Totals = make([]TaxTotal, 0, len(groups))
	for _, t := range groups {
		res.Totals = append(res.Totals, *t)
	}
	sort.Slice(res.Totals, func(a, b int) bool {
		if res.Totals[a].Jurisdiction != res.Totals[b].Jurisdiction {
			return res.Totals[a].Jurisdiction < res.Totals[b].Jurisdiction
		}
		return res.Totals[a].Category < res.Totals[b].Category
	})
	return res, nil
}

func calcLine(ln Line) (LineResult, error) {
	if ln.Rate == nil || ln.Rate.Sign() < 0 {
		return LineResult{}, errors.New("tax: rate must be non-negative")
	}
	if ln.Inclusive {
		// Base is gross; extract net = gross / (1+rate), tax = gross - net. Doing
		// the split this way (not net*rate) guarantees net + tax = gross exactly.
		onePlus := new(big.Rat).Add(big.NewRat(1, 1), ln.Rate)
		ratio := new(big.Rat).Inv(onePlus)
		net, err := ln.Base.MulRat(ratio, ln.Rounding)
		if err != nil {
			return LineResult{}, err
		}
		tax, err := ln.Base.Sub(net)
		if err != nil {
			return LineResult{}, err
		}
		return LineResult{Net: net, Tax: tax, Gross: ln.Base}, nil
	}
	// Exclusive: Base is net; tax = net*rate added on top.
	tax, err := ln.Base.MulRat(ln.Rate, ln.Rounding)
	if err != nil {
		return LineResult{}, err
	}
	gross, err := ln.Base.Add(tax)
	if err != nil {
		return LineResult{}, err
	}
	return LineResult{Net: ln.Base, Tax: tax, Gross: gross}, nil
}

// DocLine is a document line whose rate is looked up from the store, not
// supplied. Base/Inclusive carry the same meaning as Line.
type DocLine struct {
	Jurisdiction string
	Category     string
	Base         money.Money
	Inclusive    bool
}

// ResolveAndCalculate looks up each line's effective rate as of date (tenant
// override beats global), then runs the pure Calculate. This is the DB-backed
// entry invoicing uses.
func ResolveAndCalculate(ctx context.Context, db *storage.DB, tenant tenancy.ID, lines []DocLine, date time.Time) (Result, error) {
	resolved := make([]Line, len(lines))
	for i, dl := range lines {
		rr, err := RateAsOf(ctx, db, tenant, dl.Jurisdiction, dl.Category, date)
		if err != nil {
			return Result{}, err
		}
		mode, err := roundingMode(rr.Rounding)
		if err != nil {
			return Result{}, err
		}
		resolved[i] = Line{
			Jurisdiction: dl.Jurisdiction, Category: dl.Category,
			Base: dl.Base, Rate: rr.Rate, Rounding: mode, Inclusive: dl.Inclusive,
		}
	}
	return Calculate(Document{Lines: resolved})
}

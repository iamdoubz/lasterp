// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// Grain says what kind of measure a metric is, which is what makes a comparison
// meaningful (WP-1.8-decisions.md §1):
//
//   - Flow measures movement *within* a period — revenue, expenses. Its prior
//     value is the same movement one period earlier.
//   - Stock measures a balance *as at* a moment — cash, assets, AR outstanding.
//     Its prior value is the balance at the end of the previous period.
//
// Comparing a stock as if it were a flow is the classic dashboard lie ("cash
// this month: 40k" when 40k is the entire balance), so the grain is a declared
// property of the metric rather than a habit of whoever wrote the tile.
type Grain string

const (
	GrainFlow  Grain = "flow"
	GrainStock Grain = "stock"
)

// ErrNoPeriods is returned when a period-scoped evaluation is asked for on a
// book that has no fiscal periods at all.
var ErrNoPeriods = errors.New("reporting: tenant has no fiscal periods")

// ErrUnknownPeriod is returned for a period code the tenant does not have.
var ErrUnknownPeriod = errors.New("reporting: unknown period")

// dateLayout is the YYYY-MM-DD form periods store their dates in.
const dateLayout = "2006-01-02"

// window is a resolved reporting period: the period a value covers, the one it
// is compared against, and the ordering the two came from.
type window struct {
	all    []ledger.Period
	target int // index into all
	prior  int // index into all, or -1 when target is the first period
}

// Target is the period being reported on.
func (w window) Target() ledger.Period { return w.all[w.target] }

// Prior returns the preceding period, if there is one.
func (w window) Prior() (ledger.Period, bool) {
	if w.prior < 0 {
		return ledger.Period{}, false
	}
	return w.all[w.prior], true
}

// resolveWindow finds the period to report on (the latest one when code is
// empty — "what the books are doing now") and the one before it.
func resolveWindow(ctx context.Context, db *storage.DB, tenant tenancy.ID, code string) (window, error) {
	periods, err := ledger.ListPeriods(ctx, db, tenant)
	if err != nil {
		return window{}, err
	}
	if len(periods) == 0 {
		return window{}, ErrNoPeriods
	}

	target := len(periods) - 1
	if code != "" {
		target = -1
		for i, p := range periods {
			if p.Code == code {
				target = i
				break
			}
		}
		if target < 0 {
			return window{}, fmt.Errorf("%w: %q", ErrUnknownPeriod, code)
		}
	}
	return window{all: periods, target: target, prior: target - 1}, nil
}

// scopeFor returns the Scope a metric is evaluated under for one period: the
// period code (which flow metrics fold on) and the period's end date (which
// as-of metrics like AR aging age against). One period, one instant, so a card's
// two halves cannot describe different moments.
func (w window) scopeFor(index int, currency string) Scope {
	p := w.all[index]
	return Scope{Currency: currency, Period: p.Code, AsOf: endOf(p)}
}

// endOf parses a period's end date, falling back to the far future if it is
// unparseable — an as-of filter that silently excluded everything would make a
// populated book look empty.
func endOf(p ledger.Period) time.Time {
	at, err := time.Parse(dateLayout, p.EndDate)
	if err != nil {
		return time.Now().UTC()
	}
	// The stored date is the last day the period covers, so the as-of instant is
	// the end of that day rather than its midnight start.
	return at.AddDate(0, 0, 1).Add(-time.Nanosecond)
}

// loadScoped assembles report Data for a metric of the given grain.
//
// With no period in scope it is the all-time projection — the existing behaviour
// of every report and of /api/v1/metrics, kept exactly so a dashboard tile and a
// statement agree when neither is period-scoped.
func loadScoped(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Scope, grain Grain) (Data, error) {
	if s.Period == "" {
		return Load(ctx, db, tenant, s.Currency)
	}

	periods, err := ledger.ListPeriods(ctx, db, tenant)
	if err != nil {
		return Data{}, err
	}
	include, err := periodFilter(periods, s.Period, grain)
	if err != nil {
		return Data{}, err
	}
	balances, err := ledger.BalancesForPeriods(ctx, db, tenant, include)
	if err != nil {
		return Data{}, fmt.Errorf("reporting: period balances: %w", err)
	}
	accounts, err := loadAccounts(ctx, db, tenant)
	if err != nil {
		return Data{}, err
	}
	return Data{Accounts: accounts, Balances: balances, Currency: s.Currency}, nil
}

// periodFilter builds the fold predicate for a grain and target period: a flow
// sees exactly its own period, a stock sees everything up to and including it.
//
// A period code the tenant no longer has (its Period record was archived after
// entries posted to it) cannot be placed in time. Such an entry is included in
// every stock balance — the money is in the account whether or not we can order
// the period — and in no flow, because attributing the movement to a period is
// precisely what we cannot do.
func periodFilter(periods []ledger.Period, code string, grain Grain) (func(string) bool, error) {
	order := make(map[string]int, len(periods))
	target := -1
	for i, p := range periods {
		order[p.Code] = i
		if p.Code == code {
			target = i
		}
	}
	if target < 0 {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPeriod, code)
	}

	if grain == GrainFlow {
		return func(p string) bool { return p == code }, nil
	}
	return func(p string) bool {
		i, known := order[p]
		return !known || i <= target
	}, nil
}

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

// The metrics layer (docs/21 §1). Its guarantee is the reason it exists at all:
// "the CEO's revenue number and the CFO's revenue number can never disagree" —
// one definition, one evaluator, one permission check, feeding reports,
// dashboards, exports and MCP alike.
//
// v1 ships the SHAPE and the governance, not the volume. Definitions carry the
// metadata docs/21 describes (label, unit, direction, owner, required
// permission) but bind to a Go evaluator rather than a parsed formula string:
// dimensions, grains, comparisons and the formula DSL are the builder's problem
// (WP-4.13), and a formula parser is its own WP. Tenant- and plugin-authored
// metrics arrive with it — the registry is what makes that additive rather than
// a rewrite (WP-1.6-decisions.md §6).

// Unit describes how a metric value should be rendered. Formatting itself is
// the client's job (docs/17 locale rules); this says which kind of number it is.
type Unit string

const (
	UnitMoneyMinor Unit = "money_minor" // integer minor units + currency
	UnitDays       Unit = "days"
	UnitCount      Unit = "count"
)

// Direction says which way is good, so a dashboard can colour a delta without
// hardcoding per-metric knowledge (docs/21 §1 good_direction).
type Direction string

const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
)

// Metric is a certified measure. Object+Action is the permission the caller must
// hold: the engine checks it before evaluating, so a metric can never compute
// over data its viewer cannot read (docs/21 §1, INV-T1).
type Metric struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	Unit      Unit      `json:"unit"`
	Direction Direction `json:"good_direction"`
	Owner     string    `json:"owner"`
	Object    string    `json:"object"`
	Action    string    `json:"action"`

	// evaluate computes the value. Unexported so a metric cannot be redefined
	// from outside the registry — the single-definition guarantee is only worth
	// something if it cannot be quietly overridden.
	evaluate func(context.Context, *storage.DB, tenancy.ID, Scope) (int64, error)
}

// Scope is what a report run or metric evaluation is scoped by. Reports and
// metrics share one type deliberately: they answer questions about the same
// books over the same slice, and two structurally identical types would only
// invite them to drift. v1 supports currency and an as-of date; dimensions land
// with the builder.
type Scope struct {
	Currency string
	AsOf     time.Time
}

// Value is one evaluated metric.
type Value struct {
	Metric   string `json:"metric"`
	Label    string `json:"label"`
	Unit     Unit   `json:"unit"`
	Currency string `json:"currency,omitempty"`
	Value    int64  `json:"value"`
}

// ErrUnknownMetric is returned for a name that is not in the registry.
var ErrUnknownMetric = errors.New("reporting: unknown metric")

// registry is the certified metric catalog, keyed by name.
var registry = map[string]Metric{}

func register(m Metric) {
	if _, dup := registry[m.Name]; dup {
		// Two definitions of one name is exactly the disagreement the layer
		// exists to prevent; fail at init, not at read time.
		panic("reporting: duplicate metric definition " + m.Name)
	}
	registry[m.Name] = m
}

func init() {
	register(Metric{
		Name: "ar_outstanding", Label: "Accounts receivable outstanding",
		Unit: UnitMoneyMinor, Direction: DirectionDown, Owner: "module.invoicing",
		Object: invoicing.ObjectInvoice, Action: "read",
		evaluate: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Scope) (int64, error) {
			items, err := OpenItems(ctx, db, tenant, s.Currency, s.AsOf)
			if err != nil {
				return 0, err
			}
			var total int64
			for _, it := range items {
				total += it.OutstandingMinor
			}
			return total, nil
		},
	})

	register(Metric{
		Name: "ar_overdue", Label: "Accounts receivable overdue",
		Unit: UnitMoneyMinor, Direction: DirectionDown, Owner: "module.invoicing",
		Object: invoicing.ObjectInvoice, Action: "read",
		evaluate: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Scope) (int64, error) {
			items, err := OpenItems(ctx, db, tenant, s.Currency, s.AsOf)
			if err != nil {
				return 0, err
			}
			var total int64
			for _, it := range items {
				if it.AgeDays > 0 {
					total += it.OutstandingMinor
				}
			}
			return total, nil
		},
	})

	register(Metric{
		Name: "open_invoice_count", Label: "Open invoices",
		Unit: UnitCount, Direction: DirectionDown, Owner: "module.invoicing",
		Object: invoicing.ObjectInvoice, Action: "read",
		evaluate: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Scope) (int64, error) {
			items, err := OpenItems(ctx, db, tenant, s.Currency, s.AsOf)
			if err != nil {
				return 0, err
			}
			return int64(len(items)), nil
		},
	})

	// Ledger metrics all read the same Data the statements do, so a dashboard
	// tile and the P&L can never disagree about revenue.
	ledgerMetric := func(name, label string, dir Direction, pick func(Data) int64) Metric {
		return Metric{
			Name: name, Label: label, Unit: UnitMoneyMinor, Direction: dir,
			Owner: "module.ledger", Object: ledger.ObjectJournalEntry, Action: "read",
			evaluate: func(ctx context.Context, db *storage.DB, tenant tenancy.ID, s Scope) (int64, error) {
				d, err := Load(ctx, db, tenant, s.Currency)
				if err != nil {
					return 0, err
				}
				return pick(d), nil
			},
		}
	}
	register(ledgerMetric("revenue", "Revenue", DirectionUp, func(d Data) int64 {
		return sectionTotal(d, ledger.AccountIncome)
	}))
	register(ledgerMetric("expenses", "Expenses", DirectionDown, func(d Data) int64 {
		return sectionTotal(d, ledger.AccountExpense)
	}))
	register(ledgerMetric("net_income", "Net income", DirectionUp, NetIncome))
	register(ledgerMetric("total_assets", "Total assets", DirectionUp, func(d Data) int64 {
		return sectionTotal(d, ledger.AccountAsset)
	}))
	register(ledgerMetric("total_liabilities", "Total liabilities", DirectionDown, func(d Data) int64 {
		return sectionTotal(d, ledger.AccountLiability)
	}))
	register(ledgerMetric("cash_position", "Cash position", DirectionUp, func(d Data) int64 {
		return sectionTotal(d, ledger.AccountAsset)
	}))
}

// sectionTotal sums one statement section, sign-normalized for presentation.
func sectionTotal(d Data, section string) int64 {
	var total int64
	for _, a := range d.Accounts {
		if classify(a.Type) == section {
			total += presentation(a.Type, d.netFor(a.ID))
		}
	}
	return total
}

// Metrics lists the certified catalog, sorted. It does NOT filter by permission:
// knowing a metric exists is not knowing its value, and the catalog is the same
// for every tenant. Evaluation is where the gate is.
func Metrics() []Metric {
	out := make([]Metric, 0, len(registry))
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Evaluate computes one metric, after checking the permission its definition
// declares. This is the gate docs/21 §1 requires to live in the engine rather
// than the UI: a caller who cannot read invoices cannot learn AR outstanding by
// asking for it as a metric instead (INV-T1).
func Evaluate(ctx context.Context, db *storage.DB, tenant tenancy.ID, name string, scope Scope) (Value, error) {
	m, ok := registry[name]
	if !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrUnknownMetric, name)
	}
	if err := authorizeRead(ctx, db, m.Object); err != nil {
		return Value{}, err
	}
	v, err := m.evaluate(ctx, db, tenant, scope)
	if err != nil {
		return Value{}, err
	}
	out := Value{Metric: m.Name, Label: m.Label, Unit: m.Unit, Value: v}
	if m.Unit == UnitMoneyMinor {
		out.Currency = scope.Currency
	}
	return out, nil
}

// EvaluateAll computes every metric the caller is permitted to see, skipping
// those they are not. A dashboard renders what the viewer may know and simply
// omits the rest — it must not reveal a value, and it must not fail the whole
// page because one tile is off-limits.
func EvaluateAll(ctx context.Context, db *storage.DB, tenant tenancy.ID, scope Scope) ([]Value, error) {
	var out []Value
	for _, m := range Metrics() {
		v, err := Evaluate(ctx, db, tenant, m.Name, scope)
		if err != nil {
			if isPermissionDenied(err) {
				continue
			}
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

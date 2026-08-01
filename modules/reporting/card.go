// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"errors"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// A KPI card is a number plus the context that makes it mean something. docs/21
// §3: "KPI cards always render actual vs. target/budget/prior period with delta
// — a lone '4.2M' is impossible by default."
//
// v1 compares against the prior period. Target and budget need a budget object
// and the goals module (docs/12, Phase 4) — there is nothing to read yet, and a
// card claiming a target it invented would be worse than one that says which
// basis it used (WP-1.8-decisions.md §8).

// BasisPriorPeriod names the comparison basis on every card, so a reader never
// has to assume which one they are looking at.
const BasisPriorPeriod = "prior_period"

// Comparison is the "vs what" half of a card.
type Comparison struct {
	Basis  string `json:"basis"`
	Period string `json:"period"`
	Value  int64  `json:"value"`
	// DeltaMinor is current − prior in the metric's own unit.
	DeltaMinor int64 `json:"delta_minor"`
	// DeltaBasisPoints is the relative change in hundredths of a percent, nil
	// when the prior value is zero (a percentage change from nothing is not a
	// number). Basis points keep this exact: a float percentage in a money path
	// is precisely what INV-F4 exists to prevent, and 12.34% is 1234 bp.
	DeltaBasisPoints *int64 `json:"delta_basis_points,omitempty"`
	// Improved says whether the delta moved in the metric's good direction, so
	// a client colours from the definition rather than guessing that "up is
	// good". Nil when the value did not change.
	Improved *bool `json:"improved,omitempty"`
}

// Card is one KPI tile: an evaluated metric and its comparison.
//
// Its fields are spelled out rather than embedding Value, so `card.Value` is
// the number and not a struct that happens to contain one.
type Card struct {
	Metric   string `json:"metric"`
	Label    string `json:"label"`
	Unit     Unit   `json:"unit"`
	Grain    Grain  `json:"grain"`
	Currency string `json:"currency,omitempty"`
	Period   string `json:"period,omitempty"`
	Value    int64  `json:"value"`
	// Direction is the metric's good_direction, so the client colours a delta
	// from the definition instead of assuming that up is good.
	Direction  Direction   `json:"good_direction"`
	Comparison *Comparison `json:"comparison,omitempty"`
}

// cardFrom lifts an evaluated metric into a card.
func cardFrom(v Value, direction Direction) Card {
	return Card{
		Metric: v.Metric, Label: v.Label, Unit: v.Unit, Grain: v.Grain,
		Currency: v.Currency, Period: v.Period, Value: v.Value, Direction: direction,
	}
}

// compareCard evaluates a metric for a period and against the period before it.
//
// A missing comparison is honest and expected — the first period a book has, or
// a metric whose prior value could not be evaluated — and the card renders
// without one rather than inventing a zero to compare against, which would show
// a confident "+100%" for every number in a new tenant's first month.
func compareCard(ctx context.Context, db *storage.DB, tenant tenancy.ID, name string, w window, currency string) (Card, error) {
	m, err := lookup(name)
	if err != nil {
		return Card{}, err
	}

	current, err := Evaluate(ctx, db, tenant, name, w.scopeFor(w.target, currency))
	if err != nil {
		return Card{}, err
	}
	card := cardFrom(current, m.Direction)

	prior, ok := w.Prior()
	if !ok {
		return card, nil
	}
	previous, err := Evaluate(ctx, db, tenant, name, w.scopeFor(w.prior, currency))
	if err != nil {
		if priorFailureIsBenign(err) {
			return card, nil
		}
		return Card{}, err
	}

	card.Comparison = compare(prior.Code, card.Value, previous.Value, m.Direction)
	return card, nil
}

// priorFailureIsBenign reports whether a failure to evaluate the prior period
// means "there is legitimately no number to compare against" rather than "the
// system is broken".
//
// The distinction is the whole point. Until WP-1.10 this function did not exist
// and compareCard swallowed *every* error, so a database that had fallen over
// rendered as "no comparison available" on every card, indefinitely and
// silently — the failure mode a dashboard is least able to survive, because a
// missing comparison is indistinguishable from a new tenant's first month
// (phase-1-review.md P1). Only these two sentinels mean the former.
func priorFailureIsBenign(err error) bool {
	return errors.Is(err, ErrNoPeriods) || errors.Is(err, ErrUnknownPeriod)
}

// compare builds the comparison arithmetic. Kept separate from evaluation so the
// signs, the zero cases and the basis-point rounding can be tested without a
// database.
func compare(period string, current, prior int64, direction Direction) *Comparison {
	c := &Comparison{
		Basis:      BasisPriorPeriod,
		Period:     period,
		Value:      prior,
		DeltaMinor: current - prior,
	}
	if prior != 0 {
		// Relative to the magnitude of the prior value: a move from −100 to −50
		// is a 50% improvement, not −50%.
		magnitude := prior
		if magnitude < 0 {
			magnitude = -magnitude
		}
		bp := (current - prior) * 10000 / magnitude
		c.DeltaBasisPoints = &bp
	}
	if c.DeltaMinor != 0 {
		improved := (c.DeltaMinor > 0) == (direction == DirectionUp)
		c.Improved = &improved
	}
	return c
}

// cardsAsOf reports the instant a rendered card set actually reflects, so a
// dashboard can show its freshness instead of implying it is live (dashboards do
// not subscribe to the change feed until Phase 2 — WP-1.8-decisions.md §8).
//
// It is clamped to now: the current period ends in the future, and stamping a
// dashboard "as of 31 July" on the 3rd would claim data that does not exist yet.
func cardsAsOf(w window, now time.Time) time.Time {
	end := endOf(w.Target())
	if end.After(now) {
		return now
	}
	return end
}

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"testing"
	"time"
)

// The comparison arithmetic is where a dashboard lies most easily: a sign
// flipped, a percentage taken against zero, "improved" hardcoded to mean "up".
// It is pure, so it is tested without a database.
func TestCompareArithmetic(t *testing.T) {
	bp := func(n int64) *int64 { return &n }
	yes, no := true, false

	tests := []struct {
		name      string
		current   int64
		prior     int64
		direction Direction
		want      Comparison
	}{
		{
			name:    "growth on an up-is-good metric is an improvement",
			current: 12000, prior: 10000, direction: DirectionUp,
			want: Comparison{Value: 10000, DeltaMinor: 2000, DeltaBasisPoints: bp(2000), Improved: &yes},
		},
		{
			name:    "the same growth on a down-is-good metric is not",
			current: 12000, prior: 10000, direction: DirectionDown,
			want: Comparison{Value: 10000, DeltaMinor: 2000, DeltaBasisPoints: bp(2000), Improved: &no},
		},
		{
			name:    "a fall on a down-is-good metric is an improvement",
			current: 8000, prior: 10000, direction: DirectionDown,
			want: Comparison{Value: 10000, DeltaMinor: -2000, DeltaBasisPoints: bp(-2000), Improved: &yes},
		},
		{
			// A percentage change from nothing is not a number. Rendering
			// "+100%" for every first-month figure would be confident nonsense.
			// The move still has a direction — going from nothing to something
			// is an improvement — but no percentage.
			name:    "a zero prior has a delta and a direction but no percentage",
			current: 5000, prior: 0, direction: DirectionUp,
			want: Comparison{Value: 0, DeltaMinor: 5000, Improved: &yes},
		},
		{
			// Relative to the magnitude: −100 → −50 is a 50% improvement, and
			// dividing by the signed prior would call it −50%.
			name:    "a negative prior compares against its magnitude",
			current: -5000, prior: -10000, direction: DirectionUp,
			want: Comparison{Value: -10000, DeltaMinor: 5000, DeltaBasisPoints: bp(5000), Improved: &yes},
		},
		{
			name:    "no change is neither an improvement nor a regression",
			current: 10000, prior: 10000, direction: DirectionUp,
			want: Comparison{Value: 10000, DeltaMinor: 0, DeltaBasisPoints: bp(0)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compare("2026-06", tc.current, tc.prior, tc.direction)
			if got.Basis != BasisPriorPeriod {
				t.Errorf("basis = %q, want %q — a card must say what it compares against", got.Basis, BasisPriorPeriod)
			}
			if got.Period != "2026-06" {
				t.Errorf("period = %q, want the prior period's code", got.Period)
			}
			if got.Value != tc.want.Value || got.DeltaMinor != tc.want.DeltaMinor {
				t.Errorf("value/delta = %d/%d, want %d/%d", got.Value, got.DeltaMinor, tc.want.Value, tc.want.DeltaMinor)
			}
			assertPtr(t, "delta_basis_points", got.DeltaBasisPoints, tc.want.DeltaBasisPoints)
			assertBoolPtr(t, "improved", got.Improved, tc.want.Improved)
		})
	}
}

func assertPtr(t *testing.T, field string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", field, fmtPtr(got), fmtPtr(want))
	case *got != *want:
		t.Errorf("%s = %d, want %d", field, *got, *want)
	}
}

func assertBoolPtr(t *testing.T, field string, got, want *bool) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", field, got, want)
	case *got != *want:
		t.Errorf("%s = %t, want %t", field, *got, *want)
	}
}

func fmtPtr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// The current period ends in the future, so an unclamped "as of" would stamp a
// dashboard with a date whose data does not exist yet.
func TestCardsAsOfIsClampedToNow(t *testing.T) {
	w := window{all: testPeriods("2026-07"), target: 0, prior: -1}

	early := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	if got := cardsAsOf(w, early); !got.Equal(early) {
		t.Errorf("as-of mid-period = %s, want now (%s)", got, early)
	}

	later := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got := cardsAsOf(w, later); !got.Before(later) {
		t.Errorf("as-of after the period closed = %s, want the period end", got)
	}
}

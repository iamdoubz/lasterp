// SPDX-License-Identifier: AGPL-3.0-only

package money

import "math/big"

// RoundingMode selects how a fractional minor unit is rounded to an integer.
type RoundingMode int

const (
	// HalfEven rounds to the nearest integer, ties to the even neighbour
	// (banker's rounding) — the least-biased default over many operations
	// (WP-1.1 decisions, decision 4).
	HalfEven RoundingMode = iota
	// HalfUp rounds to the nearest integer, ties away from zero. Some tax
	// jurisdictions mandate it; the tax engine (WP-1.3) selects per rule.
	HalfUp
)

// roundRat rounds an exact rational to the nearest integer using mode. All
// money rounding goes through here so the policy lives in one place and never
// touches a float.
func roundRat(r *big.Rat, mode RoundingMode) *big.Int {
	num, den := r.Num(), r.Denom() // den > 0 (big.Rat is normalised)
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem) // q truncates toward zero; rem carries num's sign
	if rem.Sign() == 0 {
		return q
	}

	twiceRem := new(big.Int).Lsh(new(big.Int).Abs(rem), 1) // 2*|rem|
	cmp := twiceRem.Cmp(den)                               // compare |frac| to 1/2

	roundAway := false
	switch mode {
	case HalfUp:
		roundAway = cmp >= 0
	case HalfEven:
		if cmp > 0 {
			roundAway = true
		} else if cmp == 0 {
			roundAway = q.Bit(0) == 1 // tie: step only if q is odd (→ even)
		}
	}
	if roundAway {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

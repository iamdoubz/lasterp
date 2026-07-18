// SPDX-License-Identifier: AGPL-3.0-only

package money

import (
	"errors"
	"fmt"
	"math/big"
)

// MulRat returns m scaled by ratio, rounded to m's minor unit via mode, keeping
// the same currency. All arithmetic is exact rational (math/big) — no float in
// the money path (INV-F4). It is the same-currency sibling of Convert: where
// Convert changes the currency (and applies the minor-unit scale difference),
// MulRat applies a pure ratio (a tax rate, a discount fraction) within one
// currency. ratio must be non-negative (a tax/discount rate is never negative;
// a zero ratio yields a zero amount, e.g. an exempt tax line).
func (m Money) MulRat(ratio *big.Rat, mode RoundingMode) (Money, error) {
	if ratio == nil || ratio.Sign() < 0 {
		return Money{}, errors.New("money: MulRat ratio must be non-negative")
	}
	val := new(big.Rat).Mul(new(big.Rat).SetInt64(m.amount), ratio)
	q := roundRat(val, mode)
	if !q.IsInt64() {
		return Money{}, fmt.Errorf("money: MulRat of %s overflows int64 minor units", m)
	}
	return Money{amount: q.Int64(), currency: m.currency}, nil
}

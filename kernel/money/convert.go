// SPDX-License-Identifier: AGPL-3.0-only

package money

import (
	"errors"
	"fmt"
	"math/big"
)

// Convert changes m into target currency at rate (units of target per unit of
// m's currency), rounding to target's minor unit via mode. All arithmetic is
// exact rational (math/big) — no float in the money path (INV-F4). The minor-
// unit scale difference between currencies (USD=2, JPY=0) is applied so the
// result is correct across currencies with different decimal places.
func Convert(m Money, rate *big.Rat, target string, mode RoundingMode) (Money, error) {
	src, err := Lookup(m.currency)
	if err != nil {
		return Money{}, err
	}
	tgt, err := Lookup(target)
	if err != nil {
		return Money{}, err
	}
	if rate == nil || rate.Sign() <= 0 {
		return Money{}, errors.New("money: Convert rate must be positive")
	}

	// value = amount * rate * 10^(tgt.Digits - src.Digits)
	val := new(big.Rat).SetInt64(m.amount)
	val.Mul(val, rate)
	switch diff := tgt.Digits - src.Digits; {
	case diff > 0:
		val.Mul(val, new(big.Rat).SetInt(pow10Big(diff)))
	case diff < 0:
		val.Quo(val, new(big.Rat).SetInt(pow10Big(-diff)))
	}

	q := roundRat(val, mode)
	if !q.IsInt64() {
		return Money{}, fmt.Errorf("money: converting %s to %s overflows int64 minor units", m, tgt.Code)
	}
	return Money{amount: q.Int64(), currency: tgt.Code}, nil
}

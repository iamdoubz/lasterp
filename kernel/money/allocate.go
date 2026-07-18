// SPDX-License-Identifier: AGPL-3.0-only

package money

import (
	"errors"
	"math/big"
	"sort"
)

// Allocate splits m across len(ratios) parts in proportion to ratios, losing
// no minor unit: the parts always sum back to m exactly (INV-F4). It uses the
// largest-remainder method — floor each part, then hand the leftover minor
// units one each to the parts with the largest fractional remainders
// (deterministic; ties go to the lower index). Negative amounts are allocated
// by magnitude then sign-applied, so conservation holds for either sign.
func (m Money) Allocate(ratios []int64) ([]Money, error) {
	if len(ratios) == 0 {
		return nil, errors.New("money: Allocate needs at least one ratio")
	}
	var total int64
	for _, r := range ratios {
		if r < 0 {
			return nil, errors.New("money: Allocate ratios must be non-negative")
		}
		total += r
	}
	if total == 0 {
		return nil, errors.New("money: Allocate ratios sum to zero")
	}

	neg := m.amount < 0
	abs := m.amount
	if neg {
		abs = -abs
	}
	absBig, totalBig := big.NewInt(abs), big.NewInt(total)

	parts := make([]Money, len(ratios))
	remainders := make([]*big.Int, len(ratios))
	var distributed int64
	for i, r := range ratios {
		// abs*r via big.Int so a large amount can't overflow int64 mid-product.
		prod := new(big.Int).Mul(absBig, big.NewInt(r))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(prod, totalBig, rem) // floor: all operands non-negative
		parts[i] = Money{amount: q.Int64(), currency: m.currency}
		remainders[i] = rem
		distributed += q.Int64()
	}

	// leftover is in [0, len-1]: the minor units lost to flooring.
	leftover := abs - distributed
	order := make([]int, len(ratios))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return remainders[order[a]].Cmp(remainders[order[b]]) > 0
	})
	for k := int64(0); k < leftover; k++ {
		parts[order[k]].amount++
	}

	if neg {
		for i := range parts {
			parts[i].amount = -parts[i].amount
		}
	}
	return parts, nil
}

// AllocateEqual splits m into n as-equal-as-possible parts, conserving every
// minor unit.
func (m Money) AllocateEqual(n int) ([]Money, error) {
	if n <= 0 {
		return nil, errors.New("money: AllocateEqual needs n >= 1")
	}
	ratios := make([]int64, n)
	for i := range ratios {
		ratios[i] = 1
	}
	return m.Allocate(ratios)
}

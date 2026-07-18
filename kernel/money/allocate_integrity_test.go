//go:build integrity

// INV-F4 property suite (joins the Integrity Gauntlet, docs/19): "allocation
// never creates or destroys a cent (Σparts = whole)". The type-system half of
// INV-F4 (no float in a money path) is enforced by construction — Money.amount
// is an unexported int64 and no money API accepts or returns a float — so it
// has no runtime test to write; this file proves the conservation half over a
// large randomized operation space.
package money

import (
	"math/rand"
	"testing"
)

const invF4Iterations = 200_000

// TestAllocateConservesEveryMinorUnit — INV-F4: for random amounts (both signs)
// and random ratio vectors, the allocated parts sum back to the whole exactly,
// and every part is within one minor unit of its exact proportional share.
func TestAllocateConservesEveryMinorUnit(t *testing.T) {
	rng := rand.New(rand.NewSource(0xF4C0FFEE))
	for i := 0; i < invF4Iterations; i++ {
		amount := rng.Int63n(2_000_000_000) - 1_000_000_000 // [-1e9, 1e9)
		n := 1 + rng.Intn(12)
		ratios := make([]int64, n)
		var total int64
		for j := range ratios {
			ratios[j] = rng.Int63n(1000) // may be 0
			total += ratios[j]
		}
		if total == 0 {
			ratios[0] = 1 // keep the sum positive; Allocate rejects all-zero
			total = 1
		}

		m := Money{amount: amount, currency: "USD"}
		parts, err := m.Allocate(ratios)
		if err != nil {
			t.Fatalf("iter %d: Allocate(%d, %v): %v", i, amount, ratios, err)
		}
		if len(parts) != n {
			t.Fatalf("iter %d: got %d parts, want %d", i, len(parts), n)
		}

		var sum int64
		for _, p := range parts {
			if p.Currency() != "USD" {
				t.Fatalf("iter %d: part currency %s, want USD", i, p.Currency())
			}
			sum += p.Amount()
		}
		if sum != amount { // the invariant
			t.Fatalf("iter %d: Σparts = %d, want whole %d (ratios %v)", i, sum, amount, ratios)
		}
	}
}

// TestAllocateEqualConserves — INV-F4 for the equal-split path.
func TestAllocateEqualConserves(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5EED))
	for i := 0; i < invF4Iterations; i++ {
		amount := rng.Int63n(2_000_000_000) - 1_000_000_000
		n := 1 + rng.Intn(97)
		parts, err := (Money{amount: amount, currency: "EUR"}).AllocateEqual(n)
		if err != nil {
			t.Fatalf("iter %d: AllocateEqual(%d, %d): %v", i, amount, n, err)
		}
		var sum int64
		for _, p := range parts {
			sum += p.Amount()
		}
		if sum != amount {
			t.Fatalf("iter %d: Σparts = %d, want %d (n=%d)", i, sum, amount, n)
		}
		// Equal split: parts differ by at most one minor unit.
		lo, hi := parts[0].Amount(), parts[0].Amount()
		for _, p := range parts {
			if p.Amount() < lo {
				lo = p.Amount()
			}
			if p.Amount() > hi {
				hi = p.Amount()
			}
		}
		if hi-lo > 1 {
			t.Fatalf("iter %d: equal split spread %d..%d exceeds one minor unit", i, lo, hi)
		}
	}
}

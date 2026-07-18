package money

import (
	"math/big"
	"testing"
)

func TestRoundRat(t *testing.T) {
	cases := []struct {
		num, den         int64
		halfEven, halfUp int64
	}{
		{1, 3, 0, 0},    // 0.333…
		{2, 3, 1, 1},    // 0.666…
		{5, 2, 2, 3},    // 2.5  → even 2 / up 3
		{7, 2, 4, 4},    // 3.5  → even 4 / up 4
		{3, 2, 2, 2},    // 1.5  → even 2 / up 2
		{-5, 2, -2, -3}, // -2.5 → even -2 / up -3
		{-7, 2, -4, -4}, // -3.5 → even -4 / up -4
		{4, 1, 4, 4},    // exact integer
	}
	for _, c := range cases {
		r := big.NewRat(c.num, c.den)
		if got := roundRat(r, HalfEven).Int64(); got != c.halfEven {
			t.Errorf("roundRat(%d/%d, HalfEven) = %d, want %d", c.num, c.den, got, c.halfEven)
		}
		if got := roundRat(r, HalfUp).Int64(); got != c.halfUp {
			t.Errorf("roundRat(%d/%d, HalfUp) = %d, want %d", c.num, c.den, got, c.halfUp)
		}
	}
}

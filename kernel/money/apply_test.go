package money

import (
	"math/big"
	"testing"
)

func TestMulRat(t *testing.T) {
	must := func(m Money, err error) Money {
		t.Helper()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return m
	}
	cases := []struct {
		name  string
		base  Money
		ratio *big.Rat
		mode  RoundingMode
		want  int64
	}{
		// 10000 (=$100.00) × 8.25% = 825 cents exactly.
		{"sales tax exact", must(New(10000, "USD")), big.NewRat(825, 10000), HalfEven, 825},
		// 1999 (=$19.99) × 20% = 399.8 → 400 (both modes, not a tie).
		{"vat round up frac", must(New(1999, "USD")), big.NewRat(20, 100), HalfEven, 400},
		// Tie: 250 × 10% = 25.0 exact, no rounding.
		{"exact no round", must(New(250, "USD")), big.NewRat(10, 100), HalfUp, 25},
		// Tie at .5: 50 × 5% = 2.5 → HalfEven 2, HalfUp 3.
		{"half even tie", must(New(50, "USD")), big.NewRat(5, 100), HalfEven, 2},
		{"half up tie", must(New(50, "USD")), big.NewRat(5, 100), HalfUp, 3},
		// Zero ratio (exempt) → zero.
		{"zero ratio", must(New(9999, "USD")), big.NewRat(0, 1), HalfEven, 0},
		// JPY (0 decimals): 1000 yen × 10% = 100 yen.
		{"jpy", must(New(1000, "JPY")), big.NewRat(10, 100), HalfEven, 100},
		// Negative base (a credit note line): -10000 × 8.25% = -825.
		{"negative base", must(New(-10000, "USD")), big.NewRat(825, 10000), HalfEven, -825},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.base.MulRat(c.ratio, c.mode)
			if err != nil {
				t.Fatalf("MulRat: %v", err)
			}
			if got.Amount() != c.want {
				t.Errorf("MulRat = %d, want %d", got.Amount(), c.want)
			}
			if got.Currency() != c.base.Currency() {
				t.Errorf("MulRat currency = %s, want %s", got.Currency(), c.base.Currency())
			}
		})
	}
}

func TestMulRatNegativeRatioRejected(t *testing.T) {
	m, _ := New(100, "USD")
	if _, err := m.MulRat(big.NewRat(-1, 100), HalfEven); err == nil {
		t.Fatal("MulRat with negative ratio: want error, got nil")
	}
	if _, err := m.MulRat(nil, HalfEven); err == nil {
		t.Fatal("MulRat with nil ratio: want error, got nil")
	}
}

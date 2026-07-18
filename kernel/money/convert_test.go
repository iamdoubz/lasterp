package money

import (
	"math/big"
	"testing"
)

func TestConvertSameDigits(t *testing.T) {
	usd := mustNew(t, 1000, "USD") // $10.00
	got, err := Convert(usd, big.NewRat(9, 10), "EUR", HalfEven)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.Amount() != 900 || got.Currency() != "EUR" {
		t.Fatalf("Convert = %v, want 900 EUR", got)
	}
}

// Cross-digit: USD (2 dp) ⇄ JPY (0 dp) must account for the minor-unit scale.
func TestConvertCrossDigits(t *testing.T) {
	usd := mustNew(t, 1234, "USD") // $12.34
	jpy, err := Convert(usd, big.NewRat(150, 1), "JPY", HalfEven)
	if err != nil {
		t.Fatalf("Convert USD→JPY: %v", err)
	}
	if jpy.Amount() != 1851 || jpy.Currency() != "JPY" { // 12.34 * 150 = 1851
		t.Fatalf("USD→JPY = %v, want 1851 JPY", jpy)
	}
	back, err := Convert(jpy, big.NewRat(1, 150), "USD", HalfEven)
	if err != nil {
		t.Fatalf("Convert JPY→USD: %v", err)
	}
	if back.Amount() != 1234 { // 1851 / 150 = 12.34
		t.Fatalf("JPY→USD = %v, want 1234 USD", back)
	}
}

// The chosen rounding mode reaches the money layer: a .5 tie rounds evenly by
// default, up under HalfUp.
func TestConvertRoundingMode(t *testing.T) {
	m := mustNew(t, 5, "USD") // 5 cents
	// rate 1/10 → 0.5 minor units at target.
	even, err := Convert(m, big.NewRat(1, 10), "USD", HalfEven)
	if err != nil {
		t.Fatalf("Convert HalfEven: %v", err)
	}
	up, err := Convert(m, big.NewRat(1, 10), "USD", HalfUp)
	if err != nil {
		t.Fatalf("Convert HalfUp: %v", err)
	}
	if even.Amount() != 0 {
		t.Fatalf("HalfEven 0.5 = %d, want 0 (tie to even)", even.Amount())
	}
	if up.Amount() != 1 {
		t.Fatalf("HalfUp 0.5 = %d, want 1", up.Amount())
	}
}

func TestConvertRejectsBadRate(t *testing.T) {
	m := mustNew(t, 100, "USD")
	for _, r := range []*big.Rat{nil, big.NewRat(0, 1), big.NewRat(-1, 1)} {
		if _, err := Convert(m, r, "EUR", HalfEven); err == nil {
			t.Fatalf("Convert with rate %v: want error", r)
		}
	}
	if _, err := Convert(m, big.NewRat(1, 1), "ZZZ", HalfEven); err == nil {
		t.Fatal("Convert to ZZZ: want ErrUnknownCurrency")
	}
}

package tax

import (
	"math/big"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/money"
)

func usd(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.New(minor, "USD")
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	return m
}

func eur(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.New(minor, "EUR")
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	return m
}

// Exclusive: tax added on top; net stays the base.
func TestCalculateExclusive(t *testing.T) {
	doc := Document{Lines: []Line{{
		Jurisdiction: "US-CA", Category: "sales",
		Base: usd(t, 10000), Rate: big.NewRat(725, 10000), Rounding: money.HalfUp,
	}}}
	res, err := Calculate(doc)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	lr := res.Lines[0]
	if lr.Net.Amount() != 10000 || lr.Tax.Amount() != 725 || lr.Gross.Amount() != 10725 {
		t.Fatalf("got net=%d tax=%d gross=%d, want 10000/725/10725", lr.Net.Amount(), lr.Tax.Amount(), lr.Gross.Amount())
	}
}

// Inclusive: tax extracted out of a gross price; net + tax == gross exactly
// (INV-F4 — no cent lost). Germany 19% on €11.90 gross → €10.00 net, €1.90 tax.
func TestCalculateInclusiveConservesCent(t *testing.T) {
	doc := Document{Lines: []Line{{
		Jurisdiction: "EU-DE", Category: "standard",
		Base: eur(t, 1190), Rate: big.NewRat(19, 100), Rounding: money.HalfEven, Inclusive: true,
	}}}
	res, err := Calculate(doc)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	lr := res.Lines[0]
	if lr.Net.Amount() != 1000 || lr.Tax.Amount() != 190 {
		t.Fatalf("got net=%d tax=%d, want 1000/190", lr.Net.Amount(), lr.Tax.Amount())
	}
	sum, err := lr.Net.Add(lr.Tax)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c, _ := sum.Cmp(lr.Gross); c != 0 {
		t.Fatalf("net+tax=%s != gross=%s (cent not conserved)", sum, lr.Gross)
	}
}

// A zero/exempt rate yields zero tax, gross == net.
func TestCalculateZeroRate(t *testing.T) {
	doc := Document{Lines: []Line{{
		Jurisdiction: "US-OR", Category: "sales",
		Base: usd(t, 5000), Rate: big.NewRat(0, 1), Rounding: money.HalfUp,
	}}}
	res, err := Calculate(doc)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.Lines[0].Tax.Amount() != 0 || res.Lines[0].Gross.Amount() != 5000 {
		t.Fatalf("zero rate: got tax=%d gross=%d, want 0/5000", res.Lines[0].Tax.Amount(), res.Lines[0].Gross.Amount())
	}
}

// Multiple lines in the same (jurisdiction, category) group sum into one total.
func TestCalculateTotalsGrouped(t *testing.T) {
	doc := Document{Lines: []Line{
		{Jurisdiction: "US-CA", Category: "sales", Base: usd(t, 10000), Rate: big.NewRat(725, 10000), Rounding: money.HalfUp},
		{Jurisdiction: "US-CA", Category: "sales", Base: usd(t, 3000), Rate: big.NewRat(725, 10000), Rounding: money.HalfUp},
		{Jurisdiction: "EU-DE", Category: "standard", Base: eur(t, 10000), Rate: big.NewRat(19, 100), Rounding: money.HalfEven},
	}}
	res, err := Calculate(doc)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if len(res.Totals) != 2 {
		t.Fatalf("got %d totals, want 2", len(res.Totals))
	}
	// Sorted: EU-DE before US-CA.
	if res.Totals[0].Jurisdiction != "EU-DE" || res.Totals[1].Jurisdiction != "US-CA" {
		t.Fatalf("totals not sorted: %q, %q", res.Totals[0].Jurisdiction, res.Totals[1].Jurisdiction)
	}
	// US-CA: net 13000, tax = round(10000*0.0725)+round(3000*0.0725) = 725+218 = 943 (per-line rounding).
	ca := res.Totals[1]
	if ca.Net.Amount() != 13000 || ca.Tax.Amount() != 943 {
		t.Fatalf("US-CA total: got net=%d tax=%d, want 13000/943", ca.Net.Amount(), ca.Tax.Amount())
	}
}

// A negative rate is rejected (a tax rate is never negative).
func TestCalculateNegativeRateRejected(t *testing.T) {
	doc := Document{Lines: []Line{{
		Jurisdiction: "US-CA", Category: "sales",
		Base: usd(t, 10000), Rate: big.NewRat(-5, 100), Rounding: money.HalfUp,
	}}}
	if _, err := Calculate(doc); err == nil {
		t.Fatal("negative rate: want error, got nil")
	}
}

// Mixing currencies inside one (jurisdiction, category) group is a bug — the
// group total can't sum across currencies, so it errors rather than silently
// dropping a line.
func TestCalculateCurrencyMismatchInGroup(t *testing.T) {
	doc := Document{Lines: []Line{
		{Jurisdiction: "EU-DE", Category: "standard", Base: eur(t, 10000), Rate: big.NewRat(19, 100), Rounding: money.HalfEven},
		{Jurisdiction: "EU-DE", Category: "standard", Base: usd(t, 10000), Rate: big.NewRat(19, 100), Rounding: money.HalfEven},
	}}
	if _, err := Calculate(doc); err == nil {
		t.Fatal("currency mismatch in group: want error, got nil")
	}
}

package money

import (
	"encoding/json"
	"errors"
	"testing"
)

func mustNew(t *testing.T, amount int64, code string) Money {
	t.Helper()
	m, err := New(amount, code)
	if err != nil {
		t.Fatalf("New(%d, %q): %v", amount, code, err)
	}
	return m
}

func TestNewAndAccessors(t *testing.T) {
	m := mustNew(t, 1234, "usd") // lower-case normalises
	if m.Amount() != 1234 || m.Currency() != "USD" {
		t.Fatalf("got %d %s, want 1234 USD", m.Amount(), m.Currency())
	}
	if m.IsZero() || m.Sign() != 1 {
		t.Fatalf("unexpected zero/sign for %s", m)
	}
}

func TestUnknownCurrency(t *testing.T) {
	if _, err := New(1, "ZZZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("New ZZZ: err = %v, want ErrUnknownCurrency", err)
	}
	if _, err := Parse("1.00", "ZZZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Parse ZZZ: err = %v, want ErrUnknownCurrency", err)
	}
}

func TestAddSub(t *testing.T) {
	a, b := mustNew(t, 1000, "USD"), mustNew(t, 250, "USD")
	sum, err := a.Add(b)
	if err != nil || sum.Amount() != 1250 {
		t.Fatalf("Add = %v (%v), want 1250 USD", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != 750 {
		t.Fatalf("Sub = %v (%v), want 750 USD", diff, err)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	usd, eur := mustNew(t, 100, "USD"), mustNew(t, 100, "EUR")
	if _, err := usd.Add(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add mismatch: err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Sub(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub mismatch: err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Cmp(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp mismatch: err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestParseAndString(t *testing.T) {
	cases := []struct {
		in, code, want string
		amount         int64
	}{
		{"12.34", "USD", "12.34 USD", 1234},
		{"150", "JPY", "150 JPY", 150},      // 0-decimal currency
		{"1.234", "BHD", "1.234 BHD", 1234}, // 3-decimal currency
		{"-5.00", "USD", "-5.00 USD", -500},
		{"0.00", "USD", "0.00 USD", 0},
	}
	for _, c := range cases {
		m, err := Parse(c.in, c.code)
		if err != nil {
			t.Fatalf("Parse(%q, %q): %v", c.in, c.code, err)
		}
		if m.Amount() != c.amount {
			t.Fatalf("Parse(%q) amount = %d, want %d", c.in, m.Amount(), c.amount)
		}
		if m.String() != c.want {
			t.Fatalf("String() = %q, want %q", m.String(), c.want)
		}
	}
}

func TestParseTooManyDecimals(t *testing.T) {
	if _, err := Parse("12.345", "USD"); err == nil {
		t.Fatal("Parse 12.345 USD: want error (3 dp for a 2-dp currency)")
	}
	if _, err := Parse("1.5", "JPY"); err == nil {
		t.Fatal("Parse 1.5 JPY: want error (JPY has 0 dp)")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	m := mustNew(t, -1234, "EUR")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `{"amount":-1234,"currency":"EUR"}` {
		t.Fatalf("JSON = %s", b)
	}
	var got Money
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != m {
		t.Fatalf("round-trip = %v, want %v", got, m)
	}
	// Unmarshal validates the currency.
	if err := json.Unmarshal([]byte(`{"amount":1,"currency":"ZZZ"}`), &got); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("Unmarshal bad currency: err = %v, want ErrUnknownCurrency", err)
	}
}

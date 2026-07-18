// SPDX-License-Identifier: AGPL-3.0-only

// Package money is the WP-1.1 kernel money primitive: an integer-minor-unit,
// ISO-4217 value type with rounding, allocation, currency conversion, and an
// effective-dated FX rate store. It is the single place money math happens —
// CLAUDE.md: "Money: integer minor units + ISO-4217 code. Never float.
// Rounding/allocation only through kernel/money helpers." The Money fields are
// unexported so a float amount or an unvalidated currency is unrepresentable
// outside this package's constructors (INV-F4, type-system layer, docs/19).
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Money is an amount in a currency's minor units (e.g. cents) plus its
// ISO-4217 code. The zero value is not a valid Money — build one with New,
// Zero, or Parse.
type Money struct {
	amount   int64
	currency string
}

// ErrCurrencyMismatch is returned by an operation on two different currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// New returns amount (in code's minor units) as Money, validating code.
func New(amount int64, code string) (Money, error) {
	c, err := Lookup(code)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: amount, currency: c.Code}, nil
}

// Zero returns a zero amount in code.
func Zero(code string) (Money, error) { return New(0, code) }

// Parse converts a decimal string ("12.34", "-5", "150") in code into Money,
// exactly (via math/big, never float). It rejects a value with more decimal
// places than code's minor unit allows.
func Parse(s, code string) (Money, error) {
	c, err := Lookup(code)
	if err != nil {
		return Money{}, err
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return Money{}, fmt.Errorf("money: cannot parse %q as a decimal", s)
	}
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10Big(c.Digits)))
	if !scaled.IsInt() {
		return Money{}, fmt.Errorf("money: %q has more than %d decimal place(s) for %s", s, c.Digits, c.Code)
	}
	num := scaled.Num()
	if !num.IsInt64() {
		return Money{}, fmt.Errorf("money: %q overflows int64 minor units", s)
	}
	return Money{amount: num.Int64(), currency: c.Code}, nil
}

// Amount is the value in minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency is the ISO-4217 code.
func (m Money) Currency() string { return m.currency }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// Sign returns -1, 0, or +1.
func (m Money) Sign() int {
	switch {
	case m.amount < 0:
		return -1
	case m.amount > 0:
		return 1
	default:
		return 0
	}
}

// Neg returns the negated amount.
func (m Money) Neg() Money { return Money{amount: -m.amount, currency: m.currency} }

// Mul scales the amount by an integer factor.
//
// ponytail: native int64, overflows only past ~9.2e18 minor units (92
// quadrillion major units) — no real ledger reaches it; switch to checked/big
// arithmetic here only if a use case genuinely needs amounts that large.
func (m Money) Mul(n int64) Money { return Money{amount: m.amount * n, currency: m.currency} }

// Add returns m+o, or ErrCurrencyMismatch if the currencies differ.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return Money{amount: m.amount + o.amount, currency: m.currency}, nil
}

// Sub returns m-o, or ErrCurrencyMismatch if the currencies differ.
func (m Money) Sub(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return Money{amount: m.amount - o.amount, currency: m.currency}, nil
}

// Cmp returns -1/0/+1 comparing m to o, or ErrCurrencyMismatch.
func (m Money) Cmp(o Money) (int, error) {
	if m.currency != o.currency {
		return 0, fmt.Errorf("%w: %s <=> %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// String formats the amount as a decimal plus code, e.g. "12.34 USD",
// "150 JPY".
func (m Money) String() string {
	c, err := Lookup(m.currency)
	if err != nil {
		// A Money can only exist with a valid currency; fall back defensively.
		return strconv.FormatInt(m.amount, 10) + " " + m.currency
	}
	return m.decimal(c.Digits) + " " + m.currency
}

func (m Money) decimal(digits int) string {
	if digits == 0 {
		return strconv.FormatInt(m.amount, 10)
	}
	neg := m.amount < 0
	a := m.amount
	if neg {
		a = -a
	}
	div := pow10Int(digits)
	s := fmt.Sprintf("%d.%0*d", a/div, digits, a%div)
	if neg {
		s = "-" + s
	}
	return s
}

type moneyJSON struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON emits {"amount": <minor units>, "currency": "USD"} — the stable,
// float-free shape money uses in event payloads and API bodies.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{Amount: m.amount, Currency: m.currency})
}

// UnmarshalJSON reads {"amount", "currency"} and validates the currency.
func (m *Money) UnmarshalJSON(b []byte) error {
	var j moneyJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	mm, err := New(j.Amount, j.Currency)
	if err != nil {
		return err
	}
	*m = mm
	return nil
}

// pow10Int returns 10^n as int64 (n small: currency digit counts).
func pow10Int(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// pow10Big returns 10^n as *big.Int.
func pow10Big(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

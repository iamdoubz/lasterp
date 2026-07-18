// SPDX-License-Identifier: AGPL-3.0-only

package money

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/currency"
)

// ErrUnknownCurrency is returned for a code that is not a valid ISO-4217
// currency.
var ErrUnknownCurrency = errors.New("money: unknown ISO-4217 currency")

// Currency is one ISO-4217 currency: its canonical code and the number of
// decimal places its minor unit uses (Digits), e.g. USD=2, JPY=0, BHD=3.
type Currency struct {
	Code   string
	Digits int
}

// Lookup validates code and returns its Currency. The ISO-4217 minor-unit
// data comes from golang.org/x/text/currency (already a dependency) — no
// hand-maintained table (WP-1.1 decisions, decision 2).
func Lookup(code string) (Currency, error) {
	u, err := currency.ParseISO(strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	scale, _ := currency.Standard.Rounding(u)
	return Currency{Code: u.String(), Digits: scale}, nil
}

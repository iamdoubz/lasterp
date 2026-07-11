// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import (
	"fmt"
	"math"

	"golang.org/x/text/currency"
	"golang.org/x/text/number"
)

// Number renders x as a locale-formatted decimal (grouping separators, decimal
// mark per the Printer's locale).
func (p *Printer) Number(x any) string {
	return p.p.Sprint(number.Decimal(x))
}

// Money renders an amount held as integer minor units — the canonical money
// representation, which stays the source of truth and is never used for
// arithmetic here — plus an ISO-4217 code, with locale-correct symbol
// placement and grouping. Only the final render step converts to a value
// scaled to the currency's CLDR fraction digits; the printer rounds to exactly
// that many digits, so no visible precision is lost for real-world amounts.
func (p *Printer) Money(minorUnits int64, iso4217 string) (string, error) {
	unit, err := currency.ParseISO(iso4217)
	if err != nil {
		return "", fmt.Errorf("i18n: money: parse currency %q: %w", iso4217, err)
	}
	scale, _ := currency.Standard.Rounding(unit)

	major := float64(minorUnits) / math.Pow10(scale)
	return p.p.Sprint(currency.Symbol(unit.Amount(major))), nil
}

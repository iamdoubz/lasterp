// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import (
	"fmt"
	"strings"

	"golang.org/x/text/currency"
	"golang.org/x/text/number"
)

// Number renders x as a locale-formatted decimal (grouping separators, decimal
// mark per the Printer's locale).
func (p *Printer) Number(x any) string {
	return p.p.Sprint(number.Decimal(x))
}

// Money renders an amount held as integer minor units — the canonical money
// representation — plus an ISO-4217 code, with locale-correct grouping, decimal
// mark, symbol placement and sign position.
//
// The conversion to a displayable value is exact: minor units are split into
// major and fraction parts by integer division, and the major part is formatted
// as an integer. No float appears anywhere on this path (INV-F4) — the previous
// implementation divided by a power of ten in float64, which silently loses
// digits past 2^53 and put that loss on a rendered invoice.
//
// golang.org/x/text cannot format an exact rational (internal/number's Convert
// takes ints and floats; big.Rat is a TODO upstream) and its Converter hook is
// internal, so the locale's *pattern* is obtained by rendering a zero amount in
// the same locale and currency, and the exact digits are spliced into it. That
// keeps CLDR as the source of every locale-specific choice while keeping the
// arithmetic in integers.
func (p *Printer) Money(minorUnits int64, iso4217 string) (string, error) {
	unit, err := currency.ParseISO(iso4217)
	if err != nil {
		return "", fmt.Errorf("i18n: money: parse currency %q: %w", iso4217, err)
	}
	scale, _ := currency.Standard.Rounding(unit)

	// Magnitude in uint64 so math.MinInt64 needs no special case.
	negative := minorUnits < 0
	magnitude := uint64(minorUnits)
	if negative {
		magnitude = uint64(-(minorUnits + 1)) + 1
	}

	divisor := uint64(1)
	for i := 0; i < scale; i++ {
		divisor *= 10
	}
	major, fraction := magnitude/divisor, magnitude%divisor

	pattern := p.p.Sprint(currency.Symbol(unit.Amount(0)))
	prefix, zeroDigits, suffix := splitNumeric(pattern)
	if zeroDigits == "" {
		// No digits in the rendered pattern at all: nothing to splice into, so
		// report rather than emit a number-shaped string that isn't one.
		return "", fmt.Errorf("i18n: money: locale %s has no numeric pattern for %s", p.tag, iso4217)
	}

	digits := p.p.Sprint(number.Decimal(major))
	if scale > 0 {
		separator := decimalSeparator(zeroDigits)
		if separator == "" {
			// Appending the fraction digits without a mark would turn 1.00 into
			// 100. Refuse rather than render a wrong amount.
			return "", fmt.Errorf("i18n: money: locale %s renders %s without a decimal mark", p.tag, iso4217)
		}
		digits += separator + fmt.Sprintf("%0*d", scale, fraction)
	}

	var sign string
	if negative && (major > 0 || fraction > 0) {
		sign = p.minusSign()
	}
	return sign + prefix + digits + suffix, nil
}

// Date renders an ISO storage date (YYYY-MM-DD, the canonical form dates are
// stored and transported in) using the locale's pack pattern. A locale whose
// pack carries no pattern, or an input that is not an ISO date, is returned
// unchanged: an ISO date is unambiguous, which is the right failure mode for a
// document.
func (p *Printer) Date(iso string) string {
	pattern := p.T(datePatternKey)
	if !strings.Contains(pattern, "{y}") || len(iso) < 10 || iso[4] != '-' || iso[7] != '-' {
		return iso
	}
	r := strings.NewReplacer("{y}", iso[0:4], "{m}", iso[5:7], "{d}", iso[8:10])
	return r.Replace(pattern)
}

// datePatternKey is the pack key carrying a locale's date field order. x/text
// has no stable CLDR date formatter (WP-0.7 decision), so the order travels as
// pack data — see docs/notes/WP-1.7-decisions.md §8.
const datePatternKey = "doc.date.pattern"

// minusSign asks the locale for its own minus sign rather than assuming U+002D
// (CLDR uses U+2212 in some locales).
func (p *Printer) minusSign() string {
	return strings.TrimSuffix(p.p.Sprint(number.Decimal(-1)), p.p.Sprint(number.Decimal(1)))
}

// splitNumeric divides a rendered currency pattern into everything before the
// digits, the digit run itself (including any decimal mark), and everything
// after: "€0.00" → ("€", "0.00", ""), "0,00 €" → ("", "0,00", " €").
func splitNumeric(pattern string) (prefix, digits, suffix string) {
	start, end := -1, -1
	for i, r := range pattern {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			end = i + 1
		}
	}
	if start < 0 {
		return pattern, "", ""
	}
	return pattern[:start], pattern[start:end], pattern[end:]
}

// decimalSeparator reads the decimal mark out of a rendered zero ("0,00" → ",").
// The zero pattern is all digits and one separator, so the first non-digit rune
// is it; a locale rendering no fraction digits yields "".
func decimalSeparator(zeroDigits string) string {
	for _, r := range zeroDigits {
		if r < '0' || r > '9' {
			return string(r)
		}
	}
	return ""
}

//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/currency"
	"golang.org/x/text/language"
)

// INV-F4 — "Money is integer minor units + ISO-4217; no floats anywhere in a
// money path". Localizing an amount is the one place a float is tempting: the
// display value looks like a decimal. This property asserts the render is
// lossless — every rendered amount parses back to exactly the integer minor
// units it came from, across locales, currencies, signs and the int64 extremes.
//
// The previous implementation (float64(minorUnits) / math.Pow10(scale)) fails
// this test above 2^53, which is precisely the reason it is gone: an invoice
// PDF is not a place to lose the last two digits of an amount.
func TestLocalizedMoneyIsExact(t *testing.T) {
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	locales := []language.Tag{
		language.English, language.AmericanEnglish, language.German,
		language.French, language.Japanese,
	}
	currencies := []string{"USD", "EUR", "JPY", "BHD"}

	amounts := []int64{
		0, 1, -1, 5, -5, 99, 100, -100, 999, 1000, 123456789, -123456789,
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
	}
	rng := rand.New(rand.NewSource(1707))
	for i := 0; i < 2000; i++ {
		amounts = append(amounts, rng.Int63()-rng.Int63())
	}

	for _, locale := range locales {
		printer := tr.Printer(locale)
		for _, code := range currencies {
			unit, err := currency.ParseISO(code)
			if err != nil {
				t.Fatalf("ParseISO(%s): %v", code, err)
			}
			scale, _ := currency.Standard.Rounding(unit)

			for _, minor := range amounts {
				rendered, err := printer.Money(minor, code)
				if err != nil {
					t.Fatalf("Money(%d, %s) in %s: %v", minor, code, locale, err)
				}
				got, err := parseRenderedMoney(rendered, scale)
				if err != nil {
					t.Fatalf("Money(%d, %s) in %s rendered %q: %v", minor, code, locale, rendered, err)
				}
				if got != minor {
					t.Fatalf("INV-F4: Money(%d, %s) in %s rendered %q, which reads back as %d",
						minor, code, locale, rendered, got)
				}
			}
		}
	}
}

// parseRenderedMoney recovers minor units from a rendered amount without going
// anywhere near the formatter's internals: it takes the digit run, treats the
// last separator as the decimal mark when the currency has fraction digits, and
// reassembles the integer. Anything the formatter drops, misplaces or rounds
// shows up here as a different number (or a parse failure).
func parseRenderedMoney(rendered string, scale int) (int64, error) {
	start, end := -1, -1
	for i, r := range rendered {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			end = i + 1
		}
	}
	if start < 0 {
		return 0, errNoDigits
	}
	run := rendered[start:end]

	var digits strings.Builder
	var separators []string
	for _, r := range run {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			continue
		}
		separators = append(separators, string(r))
	}

	text := digits.String()
	if scale > 0 {
		if len(separators) == 0 {
			return 0, errNoDecimalMark
		}
		// The fraction is the last `scale` digits; verify the decimal mark sits
		// exactly there rather than trusting the count alone.
		if !strings.HasSuffix(run, separators[len(separators)-1]+text[len(text)-scale:]) {
			return 0, errMisplacedDecimalMark
		}
	}

	magnitude, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, err
	}
	negative := strings.ContainsAny(rendered[:start], "-−")
	if negative {
		if magnitude > 1<<63 {
			return 0, errOutOfRange
		}
		return int64(-(magnitude - 1)) - 1, nil // exact for math.MinInt64
	}
	if magnitude > math.MaxInt64 {
		return 0, errOutOfRange
	}
	return int64(magnitude), nil
}

var (
	errNoDigits             = constError("rendered amount contains no digits")
	errNoDecimalMark        = constError("rendered amount has no decimal mark for a fractional currency")
	errMisplacedDecimalMark = constError("rendered amount's decimal mark is not before the fraction digits")
	errOutOfRange           = constError("rendered amount does not fit in int64")
)

type constError string

func (e constError) Error() string { return string(e) }

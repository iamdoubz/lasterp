// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/language"
)

func TestNumber(t *testing.T) {
	tests := []struct {
		locale string
		want   string
	}{
		{"en-US", "1,234,567.89"},
		{"de-DE", "1.234.567,89"},
	}
	for _, tc := range tests {
		t.Run(tc.locale, func(t *testing.T) {
			p := i18n.New().Printer(language.MustParse(tc.locale))
			if got := p.Number(1234567.89); got != tc.want {
				t.Errorf("Number = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMoney(t *testing.T) {
	tests := []struct {
		name   string
		locale string
		minor  int64
		code   string
		want   string
	}{
		{"usd en", "en-US", 123456, "USD", "$ 1,234.56"},
		{"usd de grouping", "de-DE", 123456, "USD", "$ 1.234,56"},
		{"eur en", "en-US", 100000, "EUR", "€ 1,000.00"},
		{"jpy zero-decimal", "en-US", 1000, "JPY", "¥ 1,000"},
		// The sign leads the whole pattern, symbol included, which is where
		// CLDR puts it ("-$5.00"); x/text's own float rendering put it between
		// symbol and digits ("$ -5.00").
		{"negative", "en-US", -500, "USD", "-$ 5.00"},
		{"negative de", "de-DE", -500, "EUR", "-€ 5,00"},
		{"negative zero is not signed", "en-US", 0, "USD", "$ 0.00"},
		// Exact at the extremes: 9223372036854775807 cents is
		// 92,233,720,368,547,758.07 — a float64 divide loses the last digits.
		{"int64 max", "en-US", 9223372036854775807, "USD", "$ 92,233,720,368,547,758.07"},
		{"int64 min", "en-US", -9223372036854775808, "USD", "-$ 92,233,720,368,547,758.08"},
		{"three-decimal currency", "en-US", 1000, "BHD", "BHD 1.000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := i18n.New().Printer(language.MustParse(tc.locale))
			got, err := p.Money(tc.minor, tc.code)
			if err != nil {
				t.Fatalf("Money: %v", err)
			}
			if got != tc.want {
				t.Errorf("Money(%d,%s) = %q, want %q", tc.minor, tc.code, got, tc.want)
			}
		})
	}
}

func TestMoneyInvalidCurrency(t *testing.T) {
	p := i18n.New().Printer(language.English)
	if _, err := p.Money(100, "NOPE"); err == nil {
		t.Fatal("expected error for invalid currency code")
	}
}

func TestDate(t *testing.T) {
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name   string
		locale language.Tag
		iso    string
		want   string
	}{
		{"english keeps ISO order", language.English, "2026-07-15", "2026-07-15"},
		{"german reorders", language.German, "2026-07-15", "15.07.2026"},
		// A locale with no pack, and anything that is not an ISO date, must come
		// back untouched: an ISO date is unambiguous, a mangled one is not.
		{"unknown locale falls back to ISO", language.Japanese, "2026-07-15", "2026-07-15"},
		{"not a date", language.German, "yesterday", "yesterday"},
		{"empty", language.German, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tr.Printer(tc.locale).Date(tc.iso); got != tc.want {
				t.Errorf("Date(%q) in %s = %q, want %q", tc.iso, tc.locale, got, tc.want)
			}
		})
	}
}

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
		{"negative", "en-US", -500, "USD", "$ -5.00"},
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

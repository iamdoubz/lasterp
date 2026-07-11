// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
	"golang.org/x/text/message/catalog"
)

func TestPrinterTranslatesAndFallsBack(t *testing.T) {
	tr := i18n.New()
	if err := tr.SetString(language.English, "greeting", "Hello"); err != nil {
		t.Fatalf("SetString en: %v", err)
	}
	if err := tr.SetString(language.French, "greeting", "Bonjour"); err != nil {
		t.Fatalf("SetString fr: %v", err)
	}

	tests := []struct {
		name string
		tag  language.Tag
		key  string
		want string
	}{
		{"english", language.English, "greeting", "Hello"},
		{"french", language.French, "greeting", "Bonjour"},
		{"missing key falls back to key", language.English, "farewell", "farewell"},
		{"missing locale falls back to key", language.German, "greeting", "greeting"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tr.Printer(tc.tag).T(tc.key)
			if got != tc.want {
				t.Errorf("T(%q) for %s = %q, want %q", tc.key, tc.tag, got, tc.want)
			}
		})
	}
}

func TestPrinterPlural(t *testing.T) {
	tr := i18n.New()
	err := tr.Set(language.English, "%d items",
		plural.Selectf(1, "%d",
			plural.One, "%d item",
			plural.Other, "%d items"))
	if err != nil {
		t.Fatalf("Set plural: %v", err)
	}

	p := tr.Printer(language.English)
	tests := []struct {
		n    int
		want string
	}{
		{1, "1 item"},
		{2, "2 items"},
		{0, "0 items"},
	}
	for _, tc := range tests {
		if got := p.T("%d items", tc.n); got != tc.want {
			t.Errorf("T(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Ensure the catalog interface type is referenced so structured-message
// registration stays part of the public surface.
var _ catalog.Message

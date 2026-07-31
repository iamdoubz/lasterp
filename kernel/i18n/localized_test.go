// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/language"
)

func TestLocalizedFor(t *testing.T) {
	value := i18n.Localized{"de": "Raketenrollschuhe", "fr": "", "es": "Patines cohete"}

	tests := []struct {
		name   string
		value  i18n.Localized
		locale language.Tag
		want   string
	}{
		{"exact", value, language.German, "Raketenrollschuhe"},
		{"regional reader gets the language", value, language.MustParse("de-AT"), "Raketenrollschuhe"},
		{"absent locale", value, language.Japanese, ""},
		{"empty translation counts as absent", value, language.French, ""},
		{"nil map", nil, language.German, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.value.For(tc.locale); got != tc.want {
				t.Errorf("For(%s) = %q, want %q", tc.locale, got, tc.want)
			}
		})
	}
}

func TestLocalizedOr(t *testing.T) {
	value := i18n.Localized{"de": "Rechnung"}
	if got := value.Or(language.German, "Invoice"); got != "Rechnung" {
		t.Errorf("Or(de) = %q, want the translation", got)
	}
	if got := value.Or(language.Japanese, "Invoice"); got != "Invoice" {
		t.Errorf("Or(ja) = %q, want the fallback", got)
	}
	if got := i18n.Localized(nil).Or(language.German, "Invoice"); got != "Invoice" {
		t.Errorf("Or on a nil map = %q, want the fallback", got)
	}
}

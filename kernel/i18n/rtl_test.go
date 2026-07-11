// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/language"
)

func TestDirection(t *testing.T) {
	tests := []struct {
		tag  string
		want i18n.Direction
	}{
		{"en", i18n.LTR},
		{"en-US", i18n.LTR},
		{"fr", i18n.LTR},
		{"de", i18n.LTR},
		{"ar", i18n.RTL},
		{"ar-EG", i18n.RTL},
		{"he", i18n.RTL},
		{"fa", i18n.RTL},
		{"ur-PK", i18n.RTL},
		{"ar-XB", i18n.RTL}, // bidi pseudo-locale
		{"zh-Hans", i18n.LTR},
		{"az-Arab", i18n.RTL}, // RTL by explicit script
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			tag := language.MustParse(tc.tag)
			if got := i18n.DirectionOf(tag); got != tc.want {
				t.Errorf("DirectionOf(%s) = %v, want %v", tc.tag, got, tc.want)
			}
			if got := i18n.IsRTL(tag); got != (tc.want == i18n.RTL) {
				t.Errorf("IsRTL(%s) = %v, want %v", tc.tag, got, tc.want == i18n.RTL)
			}
		})
	}
}

func TestDirectionString(t *testing.T) {
	if got := i18n.LTR.String(); got != "ltr" {
		t.Errorf("LTR.String() = %q, want ltr", got)
	}
	if got := i18n.RTL.String(); got != "rtl" {
		t.Errorf("RTL.String() = %q, want rtl", got)
	}
}

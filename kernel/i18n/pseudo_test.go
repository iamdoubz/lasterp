// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
)

func TestPseudo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"accents letters", "Post", "⟦Þóóşţ⟧"},
		{"wraps in brackets", "Hi", "⟦Ĥíí⟧"},
		{"preserves printf verb", "Total: %d", "⟦Ţóóţààļ: %d⟧"},
		{"preserves indexed verb", "%[1]s done", "⟦%[1]s ðóóñéé⟧"},
		{"preserves brace placeholder", "Hi {name}", "⟦Ĥíí {name}⟧"},
		{"passes through digits/space", "12 34", "⟦12 34⟧"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := i18n.Pseudo(tc.in); got != tc.want {
				t.Errorf("Pseudo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPseudoExpandsLength(t *testing.T) {
	// A string of vowels should grow (truncation-bug surface); the placeholder
	// content must be preserved exactly.
	in := "Configuration {id}"
	got := i18n.Pseudo(in)
	if !strings.Contains(got, "{id}") {
		t.Errorf("Pseudo(%q) = %q, lost placeholder", in, got)
	}
	if len([]rune(got)) <= len([]rune(in)) {
		t.Errorf("Pseudo(%q) did not expand length: %q", in, got)
	}
}

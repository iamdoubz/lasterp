// SPDX-License-Identifier: AGPL-3.0-only

package i18n_test

import (
	"testing"

	"github.com/iamdoubz/lasterp/kernel/i18n"
	"golang.org/x/text/language"
)

// TestBuiltinPacksAreComplete is the translation-pack pipeline's gate: a pack
// that ships is a pack that is finished. A future WP adding a document string
// without translating it fails here rather than printing English onto a German
// invoice (docs/notes/WP-1.7-decisions.md §2).
func TestBuiltinPacksAreComplete(t *testing.T) {
	packs, err := i18n.BuiltinPacks()
	if err != nil {
		t.Fatalf("BuiltinPacks: %v", err)
	}

	var source i18n.Pack
	for _, p := range packs {
		if p.Locale == i18n.SourceLocale {
			source = p
		}
	}
	if source.Locale == "" {
		t.Fatalf("no source pack (%s) among %d packs", i18n.SourceLocale, len(packs))
	}
	if len(packs) < 2 {
		t.Fatal("no non-source pack ships: WP-1.7 requires a first non-English pack")
	}

	for _, p := range packs {
		if p.Locale == source.Locale {
			continue
		}
		for key := range source.Messages {
			if _, ok := p.Messages[key]; !ok {
				t.Errorf("pack %s is missing key %q", p.Locale, key)
			}
		}
		for key := range p.Messages {
			if _, ok := source.Messages[key]; !ok {
				t.Errorf("pack %s has key %q that the %s source does not define", p.Locale, key, source.Locale)
			}
		}
		for key, value := range p.Messages {
			if value == "" {
				t.Errorf("pack %s has an empty translation for %q", p.Locale, key)
			}
		}
	}
}

func TestLoadRegistersEveryLocale(t *testing.T) {
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	locales := tr.Locales()
	if len(locales) == 0 {
		t.Fatal("no locales registered")
	}
	if got := locales[0].String(); got != i18n.SourceLocale {
		t.Errorf("first locale = %q, want the source locale %q (it is the matcher fallback)", got, i18n.SourceLocale)
	}

	want := map[string]bool{"en": false, "de": false, i18n.PseudoAccented.String(): false}
	for _, tag := range locales {
		if _, ok := want[tag.String()]; ok {
			want[tag.String()] = true
		}
	}
	for locale, found := range want {
		if !found {
			t.Errorf("locale %q not registered (have %v)", locale, locales)
		}
	}

	if got := tr.Printer(language.German).T("doc.invoice.title"); got != "Rechnung" {
		t.Errorf("German doc.invoice.title = %q, want %q", got, "Rechnung")
	}
	if got := tr.Printer(language.English).T("doc.invoice.title"); got != "Invoice" {
		t.Errorf("English doc.invoice.title = %q, want %q", got, "Invoice")
	}
}

// The pseudo-locale must accent prose but leave the date pattern alone: it is a
// formatting rule, and an accented one renders no date at all.
func TestPseudoLocaleAccentsProseOnly(t *testing.T) {
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := tr.Printer(i18n.PseudoAccented)

	title := p.T("doc.invoice.title")
	if title == "Invoice" {
		t.Errorf("pseudo doc.invoice.title = %q, want it accented", title)
	}
	if got := p.Date("2026-07-15"); got != "2026-07-15" {
		t.Errorf("pseudo Date = %q, want the ISO date (pattern must not be accented)", got)
	}
}

func TestMatch(t *testing.T) {
	tr, err := i18n.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name        string
		preferences []string
		want        string
	}{
		{"exact", []string{"de"}, "de"},
		{"regional falls back to its language", []string{"de-AT"}, "de"},
		{"unsupported falls through to the next preference", []string{"fr", "de"}, "de"},
		{"unsupported everywhere yields the source", []string{"fr"}, "en"},
		{"accept-language header", []string{"de-DE,de;q=0.9,en;q=0.8"}, "de"},
		{"empty preference is skipped", []string{"", "de"}, "de"},
		{"garbage is skipped, not fatal", []string{"!!!", "de"}, "de"},
		{"nothing asked", nil, "en"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tr.Match(tc.preferences...).String(); got != tc.want {
				t.Errorf("Match(%v) = %q, want %q", tc.preferences, got, tc.want)
			}
		})
	}
}

func TestPackTagAndValidation(t *testing.T) {
	packs, err := i18n.BuiltinPacks()
	if err != nil {
		t.Fatalf("BuiltinPacks: %v", err)
	}
	for _, p := range packs {
		if _, err := p.Tag(); err != nil {
			t.Errorf("pack %s: %v", p.Locale, err)
		}
		if p.Version < 1 {
			t.Errorf("pack %s: version %d, want >= 1 (packs are versioned data)", p.Locale, p.Version)
		}
	}
}

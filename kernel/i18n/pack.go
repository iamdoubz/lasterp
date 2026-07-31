// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/language"
)

// A translation pack is a versioned set of messages for one locale, shipped as
// data rather than code (docs/17: "core ships English; language packs are
// versioned packages"). The built-in packs are embedded; a pack installed at
// runtime from the registry (WP-3.2) parses through the same Pack type.
//
// These packs hold the strings the *server* renders — document text (`doc.*`)
// and per-locale formatting rules that no library supplies (`doc.date.pattern`).
// The client's UI strings are a separate pack shipped with the bundle
// (web/src/i18n/packs); see docs/notes/WP-1.7-decisions.md §2 for why a pack is
// per render target rather than one file both sides read.
//
//go:embed packs/*.json
var packFS embed.FS

// SourceLocale is the locale the source strings are authored in. Every other
// pack is checked against it for completeness (pack_test.go).
const SourceLocale = "en"

// Pack is one locale's messages.
type Pack struct {
	Locale   string            `json:"locale"`
	Version  int               `json:"version"`
	Messages map[string]string `json:"messages"`
}

// Tag parses the pack's locale.
func (p Pack) Tag() (language.Tag, error) {
	tag, err := language.Parse(p.Locale)
	if err != nil {
		return language.Und, fmt.Errorf("i18n: pack %q: parse locale: %w", p.Locale, err)
	}
	return tag, nil
}

func (p Pack) validate(filename string) error {
	if p.Locale == "" {
		return fmt.Errorf("i18n: pack %s: locale is required", filename)
	}
	if p.Version < 1 {
		return fmt.Errorf("i18n: pack %s: version must be >= 1", filename)
	}
	if len(p.Messages) == 0 {
		return fmt.Errorf("i18n: pack %s: no messages", filename)
	}
	if base := strings.TrimSuffix(path.Base(filename), ".json"); base != p.Locale {
		return fmt.Errorf("i18n: pack %s: filename does not match locale %q", filename, p.Locale)
	}
	if _, err := p.Tag(); err != nil {
		return err
	}
	return nil
}

// BuiltinPacks returns the embedded packs, sorted by locale. Callers that only
// want a ready translator should use Load.
func BuiltinPacks() ([]Pack, error) {
	entries, err := packFS.ReadDir("packs")
	if err != nil {
		return nil, err
	}
	packs := make([]Pack, 0, len(entries))
	for _, e := range entries {
		name := "packs/" + e.Name()
		data, err := packFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var p Pack
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("i18n: parse pack %s: %w", name, err)
		}
		if err := p.validate(name); err != nil {
			return nil, err
		}
		packs = append(packs, p)
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].Locale < packs[j].Locale })
	return packs, nil
}

// Load builds a Translator from the built-in packs, plus the accented
// pseudo-locale derived from the source pack. It is the one call a server makes
// at boot.
//
// The pseudo-locale is not decoration: rendering a document under it is how a
// string that never reached the catalog gets caught, because it comes back
// un-accented while everything around it is accented (docs/notes/WP-1.7-
// decisions.md §7).
func Load() (*Translator, error) {
	packs, err := BuiltinPacks()
	if err != nil {
		return nil, err
	}
	t := New()
	for _, p := range packs {
		if err := t.AddPack(p); err != nil {
			return nil, err
		}
		if p.Locale == SourceLocale {
			if err := t.AddPack(pseudoPack(p)); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// pseudoPack accents every message of the source pack (docs/17's ÀççéñţéÐ
// build, the server-side twin of web/src/i18n/pseudo.ts).
func pseudoPack(source Pack) Pack {
	messages := make(map[string]string, len(source.Messages))
	for k, v := range source.Messages {
		messages[k] = Pseudo(v)
	}
	// A date pattern is a formatting rule, not prose: accenting it would make
	// every pseudo-locale date unparseable for no diagnostic value.
	messages[datePatternKey] = source.Messages[datePatternKey]
	return Pack{Locale: PseudoAccented.String(), Version: source.Version, Messages: messages}
}

// AddPack registers every message in p under its locale.
func (t *Translator) AddPack(p Pack) error {
	tag, err := p.Tag()
	if err != nil {
		return err
	}
	// Sorted so a pack with a broken message fails identically on every run.
	keys := make([]string, 0, len(p.Messages))
	for k := range p.Messages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := t.SetString(tag, k, p.Messages[k]); err != nil {
			return err
		}
	}
	t.locales = append(t.locales, tag)
	return nil
}

// Locales lists the locales this Translator can render, source locale first —
// which is also the matcher's fallback.
func (t *Translator) Locales() []language.Tag {
	out := make([]language.Tag, 0, len(t.locales))
	for _, tag := range t.locales {
		if tag.String() == SourceLocale {
			out = append(out, tag)
		}
	}
	for _, tag := range t.locales {
		if tag.String() != SourceLocale {
			out = append(out, tag)
		}
	}
	return out
}

// Match picks the best available locale for an ordered list of preferences,
// each of which may be a bare tag ("de"), a regional one ("de-AT"), or an
// Accept-Language header. Unparseable and unsupported preferences are skipped;
// an empty or entirely unsupported list yields the source locale.
//
// Matching is what keeps a locale *preference* from becoming a 400: a client
// asking for "de-AT" gets the German pack, and one asking for Klingon gets
// English rather than an error page.
func (t *Translator) Match(preferences ...string) language.Tag {
	supported := t.Locales()
	if len(supported) == 0 {
		return language.Make(SourceLocale)
	}
	matcher := language.NewMatcher(supported)
	for _, pref := range preferences {
		if strings.TrimSpace(pref) == "" {
			continue
		}
		// ParseAcceptLanguage handles a bare tag too, so one path covers both a
		// ?locale= value and an Accept-Language header.
		tags, _, err := language.ParseAcceptLanguage(pref)
		if err != nil || len(tags) == 0 {
			continue
		}
		if _, index, conf := matcher.Match(tags...); conf > language.No {
			return supported[index]
		}
	}
	return supported[0]
}

// PrinterFor is Printer(Match(preferences…)) — the request path's one call.
func (t *Translator) PrinterFor(preferences ...string) *Printer {
	return t.Printer(t.Match(preferences...))
}

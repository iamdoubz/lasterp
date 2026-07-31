// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import "golang.org/x/text/language"

// Localized is a per-locale text value: the "multi-language data" half of
// docs/17 ("designated fields support per-locale values; documents render in
// the counterparty's language"). Keys are BCP-47 tags as authored; the empty
// map is a valid, fully-degraded value.
//
// It carries no source string of its own — the record or document field it
// belongs to already holds that, and duplicating it here would create two
// answers to "what does this say by default".
type Localized map[string]string

// For returns the value for tag: an exact match first, then the base language
// (so a "de" translation serves a "de-AT" reader), then "" — which the caller
// resolves to the field's untranslated value.
func (l Localized) For(tag language.Tag) string {
	if len(l) == 0 {
		return ""
	}
	if v, ok := l[tag.String()]; ok && v != "" {
		return v
	}
	base, _ := tag.Base()
	if v, ok := l[base.String()]; ok && v != "" {
		return v
	}
	// ponytail: no reverse widening ("de-AT" authored, "de" asked). Picking one
	// regional variant to stand for a language means picking it out of a map,
	// and which one you get would depend on iteration order.
	return ""
}

// Or returns For(tag) when a translation exists, and fallback otherwise.
func (l Localized) Or(tag language.Tag, fallback string) string {
	if v := l.For(tag); v != "" {
		return v
	}
	return fallback
}

// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import "golang.org/x/text/language"

// Direction is a writing direction. Storage and logic never branch on it; the
// UI kit uses CSS logical properties and a single dir attribute on the
// document root (docs/17: "logical CSS properties only").
type Direction int

const (
	// LTR is left-to-right (the default for most locales).
	LTR Direction = iota
	// RTL is right-to-left (Arabic, Hebrew, Persian, Urdu, …).
	RTL
)

// String returns the HTML dir attribute value ("ltr" / "rtl").
func (d Direction) String() string {
	if d == RTL {
		return "rtl"
	}
	return "ltr"
}

// rtlBase is the set of RTL-written base languages by ISO-639 code. Keyed on
// the base subtag because script tags are rarely present in real requests.
var rtlBase = map[string]bool{
	"ar":  true, // Arabic
	"he":  true, // Hebrew
	"iw":  true, // Hebrew (legacy code)
	"fa":  true, // Persian
	"ur":  true, // Urdu
	"ps":  true, // Pashto
	"sd":  true, // Sindhi
	"ug":  true, // Uyghur
	"yi":  true, // Yiddish
	"ji":  true, // Yiddish (legacy code)
	"dv":  true, // Dhivehi
	"ku":  true, // Kurdish (Sorani)
	"nqo": true, // N'Ko
}

// IsRTL reports whether tag is written right-to-left. It first honours an
// explicit script subtag (Arab/Hebr/Thaa/Nkoo/Yiii), then falls back to the
// base-language set.
func IsRTL(tag language.Tag) bool {
	if s, conf := tag.Script(); conf != language.No {
		switch s.String() {
		case "Arab", "Hebr", "Thaa", "Nkoo", "Yiii", "Syrc", "Mand", "Adlm":
			return true
		}
	}
	base, _ := tag.Base()
	return rtlBase[base.String()]
}

// DirectionOf returns the writing Direction for tag.
func DirectionOf(tag language.Tag) Direction {
	if IsRTL(tag) {
		return RTL
	}
	return LTR
}

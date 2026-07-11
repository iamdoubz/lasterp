// SPDX-License-Identifier: AGPL-3.0-only

package i18n

import (
	"strings"

	"golang.org/x/text/language"
)

// Pseudo-locale tags follow the Chrome/Android convention: en-XA is accented
// Latin (ÀççéñţéÐ), ar-XB is the bidi/RTL pseudo-locale. Rendering under these
// surfaces both un-externalized strings and truncation/RTL bugs before real
// translations exist.
var (
	// PseudoAccented (en-XA) renders accented, length-expanded text.
	PseudoAccented = language.MustParse("en-XA")
	// PseudoBidi (ar-XB) is the right-to-left pseudo-locale.
	PseudoBidi = language.MustParse("ar-XB")
)

// accentMap maps ASCII letters to visually similar accented glyphs. The result
// stays readable (bug reports remain legible) while proving the string was
// routed through the translation layer.
var accentMap = map[rune]rune{
	'a': 'à', 'b': 'ƀ', 'c': 'ç', 'd': 'ð', 'e': 'é', 'f': 'ƒ', 'g': 'ĝ',
	'h': 'ĥ', 'i': 'í', 'j': 'ĵ', 'k': 'ķ', 'l': 'ļ', 'm': 'ɱ', 'n': 'ñ',
	'o': 'ó', 'p': 'þ', 'q': 'ǫ', 'r': 'ŕ', 's': 'ş', 't': 'ţ', 'u': 'ú',
	'v': 'ṽ', 'w': 'ŵ', 'x': 'ẋ', 'y': 'ý', 'z': 'ž',
	'A': 'À', 'B': 'Ɓ', 'C': 'Ç', 'D': 'Ð', 'E': 'É', 'F': 'Ƒ', 'G': 'Ĝ',
	'H': 'Ĥ', 'I': 'Í', 'J': 'Ĵ', 'K': 'Ķ', 'L': 'Ļ', 'M': 'Ṁ', 'N': 'Ñ',
	'O': 'Ó', 'P': 'Þ', 'Q': 'Ǫ', 'R': 'Ŕ', 'S': 'Ş', 'T': 'Ţ', 'U': 'Ú',
	'V': 'Ṽ', 'W': 'Ŵ', 'X': 'Ẋ', 'Y': 'Ý', 'Z': 'Ž',
}

// Pseudo returns the accented pseudo-localization of s, wrapped in ⟦ … ⟧ and
// length-expanded ~40% (doubled vowels) to expose truncation. Format verbs
// (%d, %s, %[1]v …) and ICU/brace placeholders ({name}) are copied through
// untouched so formatting still resolves.
func Pseudo(s string) string {
	var b strings.Builder
	b.Grow(len(s)*2 + len("⟦⟧"))
	b.WriteRune('⟦')

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '%':
			// Copy a printf verb verbatim: '%' plus following non-letter
			// flags/width up to and including the terminating letter (or a
			// literal "%%").
			b.WriteRune(r)
			for i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
				if isVerbEnd(runes[i]) {
					break
				}
			}
		case '{':
			// Copy an ICU/brace placeholder verbatim up to its closing '}'.
			b.WriteRune(r)
			for i+1 < len(runes) && runes[i] != '}' {
				i++
				b.WriteRune(runes[i])
			}
		default:
			if a, ok := accentMap[r]; ok {
				b.WriteRune(a)
				if isVowel(r) {
					b.WriteRune(a) // expand length to surface truncation
				}
			} else {
				b.WriteRune(r)
			}
		}
	}

	b.WriteRune('⟧')
	return b.String()
}

func isVerbEnd(r rune) bool {
	if r == '%' {
		return true // "%%" literal
	}
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isVowel(r rune) bool {
	switch r {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return true
	}
	return false
}

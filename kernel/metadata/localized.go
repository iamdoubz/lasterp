// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/i18n"
)

// TranslationsKey is the reserved Record key carrying per-locale values for the
// object's `localized: true` fields: {field: {locale: value}}. It is a Record
// key rather than a field so that a schema's field set stays the schema's field
// set — one field, one name, plus however many languages it happens to be
// written in (docs/17, "multi-language data").
const TranslationsKey = "translations"

// translationsBlobKey is the namespace translations occupy inside the generated
// table's custom_fields blob. Field names are the blob's other keys, so it is
// reserved (see reservedFieldNames) — a field called "i18n" would overwrite
// every translation on the row.
const translationsBlobKey = "i18n"

// reservedFieldNames may not be used as field names: each would collide with
// the translation machinery's own keys.
var reservedFieldNames = map[string]bool{
	TranslationsKey:     true,
	translationsBlobKey: true,
}

// Translations maps a field name to its per-locale values.
type Translations map[string]i18n.Localized

// ErrNotLocalized is returned when a write supplies translations for a field
// that is not declared `localized: true`. Storing them anyway would mean a
// value nothing ever reads, which is worse than a rejected write: the caller
// would believe the object is translated.
var ErrNotLocalized = errors.New("metadata: field is not localized")

// localizableTypes is the closed set a field may be localized on. Localizing a
// number, a date or a money amount is a category error — those localize at
// render time from one canonical stored value (docs/17).
var localizableTypes = map[FieldType]bool{
	FieldText: true, FieldLongText: true, FieldRichText: true,
}

// localizedFields indexes the schema's localized field names.
func (c *CRUD) localizedFields() map[string]bool {
	out := make(map[string]bool)
	for _, f := range c.schema.Fields {
		if f.Localized {
			out[f.Name] = true
		}
	}
	return out
}

// translationsFrom extracts and validates the reserved translations key from a
// Record. present distinguishes "no translations supplied" (leave whatever is
// stored alone) from "an empty set supplied" (clear them).
func (c *CRUD) translationsFrom(rec Record) (translations Translations, present bool, err error) {
	raw, present := rec[TranslationsKey]
	if !present {
		return nil, false, nil
	}
	translations, err = parseTranslations(raw)
	if err != nil {
		return nil, true, err
	}
	localized := c.localizedFields()
	for field := range translations {
		if !localized[field] {
			return nil, true, fmt.Errorf("%w: %s.%s", ErrNotLocalized, c.schema.ObjectName, field)
		}
	}
	return translations, true, nil
}

// parseTranslations accepts both the Go shape and the decoded-JSON shape, since
// the same Record arrives from a Go caller and from an API request body.
func parseTranslations(raw any) (Translations, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case Translations:
		return v, nil
	case map[string]i18n.Localized:
		return Translations(v), nil
	case map[string]any:
		out := make(Translations, len(v))
		for field, byLocale := range v {
			localized, err := parseLocalized(field, byLocale)
			if err != nil {
				return nil, err
			}
			out[field] = localized
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: %q must be an object of {field: {locale: value}}", ErrValidation, TranslationsKey)
	}
}

func parseLocalized(field string, raw any) (i18n.Localized, error) {
	switch v := raw.(type) {
	case i18n.Localized:
		return v, nil
	case map[string]string:
		return i18n.Localized(v), nil
	case map[string]any:
		out := make(i18n.Localized, len(v))
		for locale, value := range v {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%w: translation %s[%s] must be a string", ErrValidation, field, locale)
			}
			out[locale] = text
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: translations for %q must be an object of {locale: value}", ErrValidation, field)
	}
}

// blobValue renders translations for storage in the custom_fields blob, or nil
// when there is nothing to store (so an untranslated row's blob stays clean
// rather than accumulating an empty object).
func (t Translations) blobValue() any {
	out := make(map[string]map[string]string, len(t))
	for field, byLocale := range t {
		values := make(map[string]string, len(byLocale))
		for locale, text := range byLocale {
			if text == "" {
				continue
			}
			values[locale] = text
		}
		if len(values) > 0 {
			out[field] = values
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// translationsFromBlob reads translations back out of a decoded custom_fields
// blob. A malformed namespace is dropped rather than failing the read: it is
// display text, and refusing to return a record because one translation is
// misshapen would take the record itself offline.
func translationsFromBlob(customFields map[string]any) Translations {
	raw, ok := customFields[translationsBlobKey]
	if !ok {
		return nil
	}
	translations, err := parseTranslations(raw)
	if err != nil || len(translations) == 0 {
		return nil
	}
	return translations
}

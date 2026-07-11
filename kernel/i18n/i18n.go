// SPDX-License-Identifier: AGPL-3.0-only

// Package i18n is LastERP's string and locale layer (docs/17). All
// user-facing strings flow through a Translator's message catalog; rendering
// localizes while storage stays canonical (UTC, ISO-4217, integer minor
// units). It is a thin layer over golang.org/x/text — the Go standard for
// CLDR/ICU plural rules and number/currency formatting — plus pseudo-locale
// generation and RTL detection for the UI-kit foundation.
package i18n

import (
	"fmt"

	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/message/catalog"
)

// Translator holds a message catalog and mints per-locale Printers. The zero
// value is not usable; construct one with New.
type Translator struct {
	builder *catalog.Builder
}

// New returns an empty Translator. Register messages with Set / SetString,
// then obtain a Printer per request locale.
func New() *Translator {
	return &Translator{builder: catalog.NewBuilder()}
}

// SetString registers a plain (non-plural) message for a locale.
func (t *Translator) SetString(tag language.Tag, key, msg string) error {
	if err := t.builder.SetString(tag, key, msg); err != nil {
		return fmt.Errorf("i18n: set string %q for %s: %w", key, tag, err)
	}
	return nil
}

// Set registers a structured message (e.g. a plural selector built with
// golang.org/x/text/feature/plural) for a locale.
func (t *Translator) Set(tag language.Tag, key string, msg ...catalog.Message) error {
	if err := t.builder.Set(tag, key, msg...); err != nil {
		return fmt.Errorf("i18n: set %q for %s: %w", key, tag, err)
	}
	return nil
}

// Printer returns a Printer bound to tag. When a key is absent for tag the
// underlying message printer falls back to the key itself, so a missing
// translation degrades to the (developer-supplied) source string rather than
// an empty render.
func (t *Translator) Printer(tag language.Tag) *Printer {
	return &Printer{
		tag: tag,
		p:   message.NewPrinter(tag, message.Catalog(t.builder)),
	}
}

// Printer renders localized strings for a single locale.
type Printer struct {
	tag language.Tag
	p   *message.Printer
}

// Tag reports the locale this Printer renders for.
func (p *Printer) Tag() language.Tag { return p.tag }

// T looks key up in the catalog and formats it with args. A missing key
// renders as the key itself formatted with args.
func (p *Printer) T(key string, args ...any) string {
	return p.p.Sprintf(key, args...)
}

// Direction reports the writing direction of this Printer's locale.
func (p *Printer) Direction() Direction { return DirectionOf(p.tag) }

// SPDX-License-Identifier: AGPL-3.0-only

// Package metadata is the WP-0.5 kernel: object schema parsing, overlay
// merging, DDL generation, and a generic runtime CRUD engine driven by the
// resulting effective schema (ADR-006). "Codegen" here means metadata-
// driven behavior produced at runtime, not emitted Go source files — see
// docs/notes/WP-0.5-decisions.md, decision 1.
package metadata

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Persistence selects how an object's data is stored. Only Persistence
// CRUD is supported by the CRUD engine in this WP — event-sourced objects
// parse and validate but codegen for them is out of scope here (decision 2).
type Persistence string

const (
	PersistenceCRUD         Persistence = "crud"
	PersistenceEventSourced Persistence = "event_sourced"
)

// FieldType is the closed set of field types the kernel defines
// (docs/03-DATA-MODEL.md) — plugins compose these, they don't invent new
// ones.
type FieldType string

const (
	FieldText     FieldType = "text"
	FieldLongText FieldType = "long_text"
	FieldRichText FieldType = "rich_text"
	FieldInt      FieldType = "int"
	FieldDecimal  FieldType = "decimal"
	FieldMoney    FieldType = "money"
	FieldCurrency FieldType = "currency"
	FieldDate     FieldType = "date"
	FieldDatetime FieldType = "datetime"
	FieldBool     FieldType = "bool"
	FieldEnum     FieldType = "enum"
	FieldLink     FieldType = "link"
	FieldTable    FieldType = "table"
	FieldJSON     FieldType = "json"
	FieldFile     FieldType = "file"
	FieldEmail    FieldType = "email"
	FieldPhone    FieldType = "phone"
	FieldAddress  FieldType = "address"
	FieldDuration FieldType = "duration"
	FieldPercent  FieldType = "percent"
	FieldComputed FieldType = "computed"
)

var validFieldTypes = map[FieldType]bool{
	FieldText: true, FieldLongText: true, FieldRichText: true, FieldInt: true,
	FieldDecimal: true, FieldMoney: true, FieldCurrency: true, FieldDate: true,
	FieldDatetime: true, FieldBool: true, FieldEnum: true, FieldLink: true,
	FieldTable: true, FieldJSON: true, FieldFile: true, FieldEmail: true,
	FieldPhone: true, FieldAddress: true, FieldDuration: true, FieldPercent: true,
	FieldComputed: true,
}

// Widget is an optional presentation override for a field, from a closed set.
// It is deliberately tiny: a widget vocabulary invented ahead of a caller is
// what WP-1.5 §2 declined to do, and inventing one here would be no better.
// Each entry has a type rule (widgetApplies) — a "radio" on a date field is a
// schema error, not a rendering surprise.
type Widget string

const (
	// WidgetTextarea renders a text-ish field as a multi-line control.
	WidgetTextarea Widget = "textarea"
	// WidgetRadio renders an enum as a radio group instead of a select.
	WidgetRadio Widget = "radio"
)

// widgetApplies[w] is the set of field types w may override.
var widgetApplies = map[Widget]map[FieldType]bool{
	WidgetTextarea: {FieldText: true, FieldLongText: true, FieldRichText: true},
	WidgetRadio:    {FieldEnum: true},
}

// Field is one field definition within an Object or Overlay.
type Field struct {
	Name     string    `yaml:"name"`
	Type     FieldType `yaml:"type"`
	Required bool      `yaml:"required"`
	Target   string    `yaml:"target,omitempty"` // link/table: the target object name
	Index    bool      `yaml:"index,omitempty"`

	// Options is the closed value set of an enum field, and is required on
	// one: an `enum` with no options is a free-text column wearing a type
	// name, which is exactly the defect WP-1.11 closes. Making it a parse
	// error rather than a lint means a module that forgets fails inside its
	// own Register() and the server refuses to boot — there is no state in
	// which a shipped enum field is unconstrained.
	//
	// An overlay may narrow this set but never widen it (see Overlay.
	// NarrowOptions): core's declaration is a ceiling, the mirror of the
	// permission floor ADR-006 puts on roles.
	Options []string `yaml:"options,omitempty"`

	// Order sorts fields for presentation: (Order, declaration index),
	// stably. Zero therefore means "keep schema order", so every schema
	// written before this existed renders unchanged and an overlay can place
	// a new field between two core ones.
	//
	// It is presentation only and NEVER reorders columns: selectColumns and
	// scanRecord walk Fields in lockstep and PlanEvolution diffs on it, so
	// the effective schema keeps declaration order and the sort happens in
	// the presentation projection. Reordering a form is not a migration.
	Order int `yaml:"order,omitempty"`

	// Group names a form section. Fields sharing a group render inside one
	// fieldset/legend, in first-appearance order. Empty means ungrouped.
	Group string `yaml:"group,omitempty"`

	// Widget overrides the control the renderer would pick from Type.
	Widget Widget `yaml:"widget,omitempty"`

	// Localized marks a text field whose value may be written in several
	// languages (docs/17: "designated fields support per-locale values"). The
	// column keeps the canonical value; the per-locale ones travel under the
	// record's reserved translations key and live in the custom_fields blob, so
	// localizing a field is not a schema migration. Only localizableTypes may
	// set it.
	Localized bool `yaml:"localized,omitempty"`

	// FromOverlay is set by Merge, never by parsing core YAML (ADR-006:
	// "Custom fields for core objects store in a JSONB column with
	// generated typed accessors" — different tenants can overlay the same
	// core object differently, so an overlay-added field cannot become a
	// fixed physical column on the one shared table every tenant uses).
	// GenerateDDL routes fields accordingly: core fields are real columns,
	// overlay fields live in the generated table's custom_fields blob.
	FromOverlay bool `yaml:"-"`
}

// Permissions maps an action (e.g. "read", "create", "update", "delete")
// to the roles allowed to perform it.
type Permissions map[string][]string

// Object is a core (or module-shipped) schema definition, parsed from the
// YAML shape in docs/03-DATA-MODEL.md. Only the subset WP-0.5's AC needs
// (fields, persistence, permissions) is acted on; workflow/sync_scope/ai
// parse and round-trip but aren't interpreted yet.
type Object struct {
	ObjectName  string      `yaml:"object"`
	Module      string      `yaml:"module"`
	Persistence Persistence `yaml:"persistence"`
	Fields      []Field     `yaml:"fields"`
	Permissions Permissions `yaml:"permissions"`
}

// ErrInvalidObject covers any schema validation failure.
var ErrInvalidObject = errors.New("metadata: invalid object schema")

// Validate checks the closed field-type set and required top-level
// attributes.
func (o *Object) Validate() error {
	if o.ObjectName == "" {
		return fmt.Errorf("%w: object name is required", ErrInvalidObject)
	}
	if o.Persistence != PersistenceCRUD && o.Persistence != PersistenceEventSourced {
		return fmt.Errorf("%w: persistence must be %q or %q, got %q", ErrInvalidObject, PersistenceCRUD, PersistenceEventSourced, o.Persistence)
	}
	if len(o.Fields) == 0 {
		return fmt.Errorf("%w: at least one field is required", ErrInvalidObject)
	}
	seen := make(map[string]bool, len(o.Fields))
	for _, f := range o.Fields {
		if err := f.validate(); err != nil {
			return err
		}
		if seen[f.Name] {
			return fmt.Errorf("%w: duplicate field name %q", ErrInvalidObject, f.Name)
		}
		seen[f.Name] = true
	}
	return nil
}

// validate checks one field definition. It is shared with overlay merging, so
// an overlay cannot introduce a field a core schema would have been refused.
func (f Field) validate() error {
	if f.Name == "" {
		return fmt.Errorf("%w: field name is required", ErrInvalidObject)
	}
	if reservedFieldNames[f.Name] {
		return fmt.Errorf("%w: field name %q is reserved (it collides with the translation keys)", ErrInvalidObject, f.Name)
	}
	if !validFieldTypes[f.Type] {
		return fmt.Errorf("%w: field %q has unknown type %q", ErrInvalidObject, f.Name, f.Type)
	}
	if f.Localized && !localizableTypes[f.Type] {
		return fmt.Errorf("%w: field %q of type %q cannot be localized (only text, long_text and rich_text can)", ErrInvalidObject, f.Name, f.Type)
	}
	if err := f.validateOptions(); err != nil {
		return err
	}
	if f.Widget != "" {
		applies, known := widgetApplies[f.Widget]
		if !known {
			return fmt.Errorf("%w: field %q has unknown widget %q", ErrInvalidObject, f.Name, f.Widget)
		}
		if !applies[f.Type] {
			return fmt.Errorf("%w: field %q of type %q cannot use widget %q", ErrInvalidObject, f.Name, f.Type, f.Widget)
		}
	}
	return nil
}

// validateOptions enforces the enum/options biconditional and the shape of the
// option list itself.
func (f Field) validateOptions() error {
	if f.Type != FieldEnum {
		if len(f.Options) > 0 {
			return fmt.Errorf("%w: field %q of type %q declares options (only enum may)", ErrInvalidObject, f.Name, f.Type)
		}
		return nil
	}
	if len(f.Options) == 0 {
		return fmt.Errorf("%w: enum field %q declares no options (an enum without a closed set is free text)", ErrInvalidObject, f.Name)
	}
	seen := make(map[string]bool, len(f.Options))
	for _, opt := range f.Options {
		if opt == "" || strings.TrimSpace(opt) != opt {
			return fmt.Errorf("%w: enum field %q has an empty or space-padded option %q", ErrInvalidObject, f.Name, opt)
		}
		if seen[opt] {
			return fmt.Errorf("%w: enum field %q repeats option %q", ErrInvalidObject, f.Name, opt)
		}
		seen[opt] = true
	}
	return nil
}

// allows reports whether v is in an enum field's option set.
func (f Field) allows(v string) bool {
	for _, opt := range f.Options {
		if opt == v {
			return true
		}
	}
	return false
}

// PresentationOrder returns the schema's fields sorted for display —
// (Order, declaration index), stably — leaving the schema's own field order
// (which storage and evolution depend on) untouched.
func (s *EffectiveSchema) PresentationOrder() []Field {
	out := append([]Field(nil), s.Fields...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

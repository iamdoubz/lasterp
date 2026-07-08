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

// Field is one field definition within an Object or Overlay.
type Field struct {
	Name     string    `yaml:"name"`
	Type     FieldType `yaml:"type"`
	Required bool      `yaml:"required"`
	Target   string    `yaml:"target,omitempty"` // link/table: the target object name
	Index    bool      `yaml:"index,omitempty"`

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
		if f.Name == "" {
			return fmt.Errorf("%w: field name is required", ErrInvalidObject)
		}
		if seen[f.Name] {
			return fmt.Errorf("%w: duplicate field name %q", ErrInvalidObject, f.Name)
		}
		seen[f.Name] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("%w: field %q has unknown type %q", ErrInvalidObject, f.Name, f.Type)
		}
	}
	return nil
}

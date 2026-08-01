// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/money"
)

// INV-T5: every stored field value conforms to its object's effective schema —
// declared type and declared option set. This file is that invariant's
// enforcement point.
//
// It validates AND normalizes, because validation alone is not enough to make
// the two dialects agree. A metadata.Record is decoded straight from JSON
// (kernel/api.decodeRecord), so a JSON integer arrives as float64 and would be
// handed to the driver as float64 for an INT column: modernc.org/sqlite absorbs
// that through type affinity, Postgres does not. Accepting float64 and storing
// int64 is the fix, and it generalizes — every rule below returns the value as
// it should be stored, not merely a verdict.
//
// Validation lives here, in the engine, rather than in the HTTP handler so that
// every write path inherits it: the API, in-process module code, the future MCP
// tool surface, and the bulk/migration paths INV-X5 forbids giving shortcuts to.

// valueRule validates one field's value and returns it normalized for storage.
// v is never nil and never the empty string — absence is the required-field
// check's business, handled before a rule is consulted.
type valueRule func(f Field, v any) (any, error)

// exactDecimal matches an exact decimal string. Money, decimal and percent are
// carried as strings end to end so a float never touches a money path (INV-F4);
// the regex is what makes "carried as a string" mean something.
var exactDecimal = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// exactInteger matches a string of integer minor units.
var exactInteger = regexp.MustCompile(`^-?\d+$`)

// valueRules is the FieldType → rule map. Its key set must equal
// validFieldTypes' (TestValueRulesCoverEveryFieldType) — that equality is the
// server-side mirror of the client renderer's assertNever, and it is the
// structural fix behind this WP's specific bug: FieldEnum became free text by
// falling through a default: branch in a switch nobody had to keep exhaustive.
var valueRules = map[FieldType]valueRule{
	FieldText:     ruleString,
	FieldLongText: ruleString,
	FieldRichText: ruleString,
	FieldPhone:    ruleString,
	FieldDuration: ruleString,
	FieldFile:     ruleString,
	FieldLink:     ruleString,
	FieldEmail:    ruleEmail,
	FieldInt:      ruleInt,
	FieldBool:     ruleBool,
	FieldDecimal:  ruleExactDecimal,
	FieldPercent:  ruleExactDecimal,
	FieldMoney:    ruleMoneyMinor,
	FieldCurrency: ruleCurrency,
	FieldDate:     ruleDate,
	FieldDatetime: ruleDatetime,
	FieldEnum:     ruleEnum,
	FieldJSON:     ruleJSON,
	FieldAddress:  ruleAddress,
	FieldTable:    ruleTable,
	FieldComputed: ruleComputed,
}

// validateValue checks v against f and returns the value to store. A nil or
// empty-string v passes through untouched: absence is required-ness, and ""
// on an optional field is how a client clears it (web submittable()).
func validateValue(f Field, v any) (any, error) {
	if v == nil || v == "" {
		return v, nil
	}
	rule, ok := valueRules[f.Type]
	if !ok {
		// Unreachable while TestValueRulesCoverEveryFieldType passes. Failing
		// closed rather than storing an unvalidated value is the point: the
		// last time this package had an unhandled field type, the result was a
		// free-text column in the chart of accounts.
		return nil, fmt.Errorf("%w: field %q has type %q with no validation rule", ErrValidation, f.Name, f.Type)
	}
	return rule(f, v)
}

// typeErr is the one shape every rejection takes, so a caller reading a
// problem+json detail always learns the field, what it got, and what it wanted.
func typeErr(f Field, v any, want string) error {
	return fmt.Errorf("%w: field %q (%s) requires %s, got %T", ErrValidation, f.Name, f.Type, want, v)
}

func ruleString(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "a string")
	}
	return s, nil
}

func ruleEmail(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "a string")
	}
	// net/mail is the stdlib's own RFC 5322 parser — a hand-rolled regex here
	// would be both longer and wronger.
	if _, err := mail.ParseAddress(s); err != nil {
		return nil, fmt.Errorf("%w: field %q is not a valid email address: %w", ErrValidation, f.Name, err)
	}
	return s, nil
}

func ruleInt(f Field, v any) (any, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		// JSON has one number type, so an integer from an API body arrives
		// here as float64. Accept it only when it is exactly an integer.
		if n != float64(int64(n)) {
			return nil, fmt.Errorf("%w: field %q requires a whole number, got %v", ErrValidation, f.Name, n)
		}
		return int64(n), nil
	default:
		return nil, typeErr(f, v, "a whole number")
	}
}

func ruleBool(f Field, v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, typeErr(f, v, "a boolean")
	}
	return b, nil
}

// ruleExactDecimal covers decimal and percent: an exact decimal string, never
// a float. A float64 is refused by name rather than by falling through the
// default, because "0.1 + 0.2" arriving from a JSON body is precisely the bug
// the integer-minor-unit rule exists to prevent (INV-F4).
func ruleExactDecimal(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "an exact decimal string (never a float)")
	}
	if !exactDecimal.MatchString(s) {
		return nil, fmt.Errorf("%w: field %q requires an exact decimal string, got %q", ErrValidation, f.Name, s)
	}
	return s, nil
}

// ruleMoneyMinor accepts a string of integer minor units. columnType still
// stores money as TEXT and openly calls itself a placeholder until the
// first-class two-column representation lands; this rule moves with it.
func ruleMoneyMinor(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "a string of integer minor units (never a float)")
	}
	if !exactInteger.MatchString(s) {
		return nil, fmt.Errorf("%w: field %q requires integer minor units, got %q", ErrValidation, f.Name, s)
	}
	return s, nil
}

// ruleCurrency validates through kernel/money's ISO-4217 registry and stores
// the canonical code. A shape check would accept "XYZ"; CLAUDE.md routes every
// money concern through kernel/money and validating a currency code is that
// registry's whole job.
func ruleCurrency(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "an ISO-4217 currency code")
	}
	c, err := money.Lookup(s)
	if err != nil {
		// Both errors are wrapped: a caller can classify this as a validation
		// failure and still reach money.ErrUnknownCurrency underneath.
		return nil, fmt.Errorf("%w: field %q: %w", ErrValidation, f.Name, err)
	}
	return c.Code, nil
}

func ruleDate(f Field, v any) (any, error) {
	return parseTime(f, v, time.DateOnly, "a YYYY-MM-DD date")
}

func ruleDatetime(f Field, v any) (any, error) {
	return parseTime(f, v, time.RFC3339, "an RFC 3339 timestamp")
}

// parseTime accepts a time.Time (an in-process caller, or a value read back
// out of the row) or a string in layout, and normalizes to UTC — CLAUDE.md:
// "Time: UTC in storage, always."
func parseTime(f Field, v any, layout, want string) (any, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case string:
		parsed, err := time.Parse(layout, t)
		if err != nil {
			return nil, fmt.Errorf("%w: field %q requires %s, got %q", ErrValidation, f.Name, want, t)
		}
		return parsed.UTC(), nil
	default:
		return nil, typeErr(f, v, want)
	}
}

func ruleEnum(f Field, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "one of "+strings.Join(f.Options, ", "))
	}
	if !f.allows(s) {
		return nil, fmt.Errorf("%w: field %q must be one of [%s], got %q",
			ErrValidation, f.Name, strings.Join(f.Options, ", "), s)
	}
	return s, nil
}

// ruleJSON accepts any JSON-representable value. A string must itself parse as
// JSON, because that is how modules store composite fields (invoicing marshals
// its lines to a string before handing them over) and an unparseable one would
// read back as garbage rather than fail at write time.
func ruleJSON(f Field, v any) (any, error) {
	if s, ok := v.(string); ok {
		if !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("%w: field %q requires valid JSON", ErrValidation, f.Name)
		}
		return s, nil
	}
	if _, err := json.Marshal(v); err != nil {
		return nil, fmt.Errorf("%w: field %q is not JSON-representable: %w", ErrValidation, f.Name, err)
	}
	return v, nil
}

// ruleAddress requires a structured value (docs/03 calls address "structured"),
// accepting either the decoded-JSON object shape or a JSON object string.
func ruleAddress(f Field, v any) (any, error) {
	switch a := v.(type) {
	case map[string]any:
		return a, nil
	case string:
		var probe map[string]any
		if err := json.Unmarshal([]byte(a), &probe); err != nil {
			return nil, fmt.Errorf("%w: field %q requires a structured address object", ErrValidation, f.Name)
		}
		return a, nil
	default:
		return nil, typeErr(f, v, "a structured address object")
	}
}

// ruleTable requires a list of child rows. GenerateDDL refuses FieldTable
// outright, so this is reachable only for event-sourced projections today.
func ruleTable(f Field, v any) (any, error) {
	if rows, ok := v.([]any); ok {
		return rows, nil
	}
	return nil, typeErr(f, v, "a list of child rows")
}

// ruleComputed refuses any supplied value: the server derives these. Accepting
// and ignoring would let a client believe it wrote something.
func ruleComputed(f Field, v any) (any, error) {
	return nil, fmt.Errorf("%w: field %q is computed and cannot be written", ErrValidation, f.Name)
}

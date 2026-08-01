package metadata

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestValueRulesCoverEveryFieldType is the structural half of this WP's fix.
//
// FieldEnum became a free-text column because columnType and scanRecord both
// end in a default: branch, so adding a field type to the closed set never had
// to be accompanied by anything. This assertion makes valueRules exhaustive by
// construction — the server-side equivalent of the web renderer's assertNever,
// which has caught the same class of omission on the client since WP-1.5.
func TestValueRulesCoverEveryFieldType(t *testing.T) {
	for ft := range validFieldTypes {
		if _, ok := valueRules[ft]; !ok {
			t.Errorf("field type %q has no validation rule — every type in the closed set needs one, "+
				"or a value of that type is stored unvalidated (INV-T5)", ft)
		}
	}
	for ft := range valueRules {
		if !validFieldTypes[ft] {
			t.Errorf("valueRules has a rule for %q, which is not in the closed field-type set", ft)
		}
	}
}

// valueCase is one field type's accepted and refused values. want is the
// normalized value the rule must return for each accepted input.
type valueCase struct {
	field    Field
	accepted []struct {
		in   any
		want any
	}
	refused []any
}

func valueCases() map[FieldType]valueCase {
	enum := Field{Name: "status", Type: FieldEnum, Options: []string{"draft", "posted"}}
	stringy := func(ft FieldType) valueCase {
		return valueCase{
			field:    Field{Name: string(ft), Type: ft},
			accepted: []struct{ in, want any }{{"hello", "hello"}},
			refused:  []any{42, true, 1.5, []any{"x"}},
		}
	}
	return map[FieldType]valueCase{
		FieldText:     stringy(FieldText),
		FieldLongText: stringy(FieldLongText),
		FieldRichText: stringy(FieldRichText),
		FieldPhone:    stringy(FieldPhone),
		FieldDuration: stringy(FieldDuration),
		FieldFile:     stringy(FieldFile),
		FieldLink:     stringy(FieldLink),

		FieldEmail: {
			field:    Field{Name: "email", Type: FieldEmail},
			accepted: []struct{ in, want any }{{"a@example.com", "a@example.com"}},
			refused:  []any{"not-an-email", "@example.com", 42},
		},
		FieldInt: {
			field: Field{Name: "count", Type: FieldInt},
			accepted: []struct{ in, want any }{
				{int(5), int64(5)},
				{int64(5), int64(5)},
				{int32(5), int64(5)},
				// JSON has one number type: an integer from an API body
				// arrives as float64 and must be stored as int64.
				{float64(5), int64(5)},
				{float64(-7), int64(-7)},
			},
			refused: []any{5.5, "5", true},
		},
		FieldBool: {
			field:    Field{Name: "flag", Type: FieldBool},
			accepted: []struct{ in, want any }{{true, true}, {false, false}},
			refused:  []any{"true", 1, 0.0},
		},
		FieldDecimal: {
			field:    Field{Name: "rate", Type: FieldDecimal},
			accepted: []struct{ in, want any }{{"0.07", "0.07"}, {"-12", "-12"}},
			refused:  []any{0.07, "abc", "1.2.3"},
		},
		FieldPercent: {
			field:    Field{Name: "pct", Type: FieldPercent},
			accepted: []struct{ in, want any }{{"19.5", "19.5"}},
			refused:  []any{19.5, "19,5"},
		},
		FieldMoney: {
			field:    Field{Name: "amount", Type: FieldMoney},
			accepted: []struct{ in, want any }{{"1999", "1999"}, {"-500", "-500"}},
			// 19.99 as a float is the exact bug INV-F4 exists to prevent.
			refused: []any{19.99, "19.99", 1999},
		},
		FieldCurrency: {
			field: Field{Name: "currency", Type: FieldCurrency},
			accepted: []struct{ in, want any }{
				{"EUR", "EUR"},
				// Normalized to the canonical ISO code.
				{"usd", "USD"},
			},
			refused: []any{"XYZ", "EURO", 978},
		},
		FieldDate: {
			field: Field{Name: "issued", Type: FieldDate},
			accepted: []struct{ in, want any }{
				{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
				{time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
			},
			refused: []any{"01/08/2026", "2026-13-01", 20260801},
		},
		FieldDatetime: {
			field: Field{Name: "at", Type: FieldDatetime},
			accepted: []struct{ in, want any }{
				{"2026-08-01T12:00:00Z", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)},
			},
			refused: []any{"2026-08-01", 0},
		},
		FieldEnum: {
			field: enum,
			accepted: []struct{ in, want any }{
				{"draft", "draft"},
				{"posted", "posted"},
			},
			// The finding, in miniature. ("" is absence, not a bad value —
			// see TestValidateValueSkipsAbsence.)
			refused: []any{"banana", "Draft", 1},
		},
		FieldJSON: {
			field: Field{Name: "lines", Type: FieldJSON},
			accepted: []struct{ in, want any }{
				{`[{"a":1}]`, `[{"a":1}]`},
				{`{}`, `{}`},
			},
			refused: []any{"{not json", "["},
		},
		FieldAddress: {
			field: Field{Name: "address", Type: FieldAddress},
			accepted: []struct{ in, want any }{
				{`{"city":"Berlin"}`, `{"city":"Berlin"}`},
			},
			refused: []any{`["Berlin"]`, "Berlin", 42},
		},
		FieldTable: {
			field:    Field{Name: "rows", Type: FieldTable},
			accepted: []struct{ in, want any }{{[]any{"a"}, []any{"a"}}},
			refused:  []any{"a", 42, map[string]any{}},
		},
		FieldComputed: {
			field: Field{Name: "total", Type: FieldComputed},
			// Server-derived: nothing is accepted.
			refused: []any{"100", 100, true},
		},
	}
}

// TestValidateValue is WP-1.11's AC 2: a wrong-typed value for each FieldType
// is refused. The completeness check keeps it honest — a new field type with
// no case here fails rather than silently going untested.
func TestValidateValue(t *testing.T) {
	cases := valueCases()
	for ft := range validFieldTypes {
		if _, ok := cases[ft]; !ok {
			t.Fatalf("field type %q has no test case; every type in the closed set needs one", ft)
		}
	}

	for ft, tc := range cases {
		t.Run(string(ft), func(t *testing.T) {
			for _, ok := range tc.accepted {
				got, err := validateValue(tc.field, ok.in)
				if err != nil {
					t.Errorf("validateValue(%q, %#v) = error %v, want accepted", ft, ok.in, err)
					continue
				}
				// reflect.DeepEqual, not !=: slices and maps are legal
				// normalized values and comparing them with != panics.
				if !reflect.DeepEqual(got, ok.want) {
					t.Errorf("validateValue(%q, %#v) = %#v, want %#v", ft, ok.in, got, ok.want)
				}
			}
			for _, bad := range tc.refused {
				if _, err := validateValue(tc.field, bad); !errors.Is(err, ErrValidation) {
					t.Errorf("validateValue(%q, %#v) = %v, want ErrValidation", ft, bad, err)
				}
			}
		})
	}
}

// TestValidateValueSkipsAbsence pins the rule that makes an out-of-set legacy
// row editable: absence is the required-field check's business, and "" on an
// optional field is how the web client clears it (submittable()).
func TestValidateValueSkipsAbsence(t *testing.T) {
	f := Field{Name: "status", Type: FieldEnum, Options: []string{"draft"}}
	for _, v := range []any{nil, ""} {
		got, err := validateValue(f, v)
		if err != nil {
			t.Errorf("validateValue(%#v) = %v, want no error", v, err)
		}
		if got != v {
			t.Errorf("validateValue(%#v) = %#v, want it passed through", v, got)
		}
	}
}

// FuzzValidateValue: docs/19's fuzzing row — malformed input to a parser may be
// rejected, never corrupt state. Here that means never panicking, whatever
// arrives from a JSON body.
func FuzzValidateValue(f *testing.F) {
	f.Add("draft")
	f.Add("2026-08-01")
	f.Add("{}")
	f.Add("\x00\xff")
	f.Add("")

	types := make([]FieldType, 0, len(validFieldTypes))
	for ft := range validFieldTypes {
		types = append(types, ft)
	}

	f.Fuzz(func(t *testing.T, s string) {
		for _, ft := range types {
			field := Field{Name: "fuzzed", Type: ft}
			if ft == FieldEnum {
				field.Options = []string{"draft", "posted"}
			}
			// Both the raw string and a few decoded-JSON shapes it could
			// arrive as. A panic here fails the test by definition.
			for _, v := range []any{s, []any{s}, map[string]any{"k": s}, float64(len(s))} {
				_, _ = validateValue(field, v)
			}
		}
	})
}

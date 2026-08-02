package metadata

import (
	"errors"
	"strings"
	"testing"
)

// enumOptionsYAML is the shape a module writes.
const enumOptionsYAML = `
object: Ticket
module: support
persistence: crud
fields:
  - {name: title, type: text, required: true}
  - {name: status, type: enum, required: true, options: [open, closed]}
permissions:
  read: [support.viewer]
`

// TestEnumWithoutOptionsIsRefused is what makes AC 1 ("every enum field in a
// shipped module declares its options") a property rather than a checklist: a
// module that forgets fails inside its own Register(), so `lasterp serve`
// refuses to boot rather than accepting free text into a typed column.
func TestEnumWithoutOptionsIsRefused(t *testing.T) {
	const bad = `
object: Ticket
module: support
persistence: crud
fields:
  - {name: status, type: enum, required: true}
permissions:
  read: [support.viewer]
`
	_, err := ParseObject([]byte(bad))
	if !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("ParseObject = %v, want ErrInvalidObject", err)
	}
	if !strings.Contains(err.Error(), "no options") {
		t.Errorf("error %q should say the enum declares no options", err)
	}
}

func TestNonEnumWithOptionsIsRefused(t *testing.T) {
	f := Field{Name: "title", Type: FieldText, Options: []string{"a"}}
	if err := f.validate(); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("validate = %v, want ErrInvalidObject", err)
	}
}

func TestOptionsMustBeUniqueAndNonEmpty(t *testing.T) {
	for name, options := range map[string][]string{
		"duplicate":    {"open", "open"},
		"empty":        {"open", ""},
		"space-padded": {"open", " closed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := Field{Name: "status", Type: FieldEnum, Options: options}
			if err := f.validate(); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("validate(%v) = %v, want ErrInvalidObject", options, err)
			}
		})
	}
}

func TestEnumOptionsParseFromYAML(t *testing.T) {
	o, err := ParseObject([]byte(enumOptionsYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	status := o.Fields[1]
	if got, want := strings.Join(status.Options, ","), "open,closed"; got != want {
		t.Fatalf("options = %q, want %q", got, want)
	}
}

// --- UI descriptors -------------------------------------------------------

func TestWidgetMustBeKnownAndApplicable(t *testing.T) {
	tests := map[string]struct {
		field   Field
		wantErr bool
	}{
		"textarea on text":  {Field{Name: "a", Type: FieldText, Widget: WidgetTextarea}, false},
		"radio on enum":     {Field{Name: "b", Type: FieldEnum, Options: []string{"x"}, Widget: WidgetRadio}, false},
		"radio on text":     {Field{Name: "c", Type: FieldText, Widget: WidgetRadio}, true},
		"textarea on int":   {Field{Name: "d", Type: FieldInt, Widget: WidgetTextarea}, true},
		"unknown widget":    {Field{Name: "e", Type: FieldText, Widget: Widget("carousel")}, true},
		"no widget is fine": {Field{Name: "f", Type: FieldDate}, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.field.validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidObject) {
				t.Errorf("err = %v, want ErrInvalidObject", err)
			}
		})
	}
}

// TestPresentationOrder is AC 4's server half: the renderer drives field order
// from the schema. Unset Order means declaration order, so every schema written
// before descriptors existed renders unchanged.
func TestPresentationOrder(t *testing.T) {
	eff := &EffectiveSchema{Object: Object{Fields: []Field{
		{Name: "c", Type: FieldText, Order: 3},
		{Name: "a", Type: FieldText, Order: 1},
		{Name: "untouched_first", Type: FieldText},
		{Name: "untouched_second", Type: FieldText},
		{Name: "b", Type: FieldText, Order: 2},
	}}}

	var got []string
	for _, f := range eff.PresentationOrder() {
		got = append(got, f.Name)
	}
	want := "untouched_first,untouched_second,a,b,c"
	if strings.Join(got, ",") != want {
		t.Fatalf("PresentationOrder = %v, want %s", got, want)
	}

	// The schema's own field order must be untouched: selectColumns and
	// scanRecord walk it in lockstep and PlanEvolution diffs on it, so
	// reordering a form must never become a migration.
	if eff.Fields[0].Name != "c" {
		t.Fatalf("PresentationOrder mutated the schema's field order: %v", eff.Fields[0].Name)
	}
}

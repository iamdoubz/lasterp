package contacts

import (
	"sort"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// TestContactKindOptionsMatchModuleConstants keeps the YAML's option list and
// validKinds from drifting. Before WP-1.11 only CreateContact checked the set,
// and the generic CRUD route bypassed it entirely — Contact.kind was the one
// enum with neither engine validation nor a report bucket to surface a bad
// value.
func TestContactKindOptionsMatchModuleConstants(t *testing.T) {
	schema, err := ContactSchema()
	if err != nil {
		t.Fatalf("ContactSchema: %v", err)
	}

	var got []string
	for _, f := range schema.Fields {
		if f.Name == "kind" {
			if f.Type != metadata.FieldEnum {
				t.Fatalf("Contact.kind is %q, not an enum", f.Type)
			}
			got = append(got, f.Options...)
		}
	}
	sort.Strings(got)

	want := make([]string, 0, len(validKinds))
	for kind := range validKinds {
		want = append(want, kind)
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Contact.kind options = %v, want %v — the schema and validKinds have drifted", got, want)
	}
}

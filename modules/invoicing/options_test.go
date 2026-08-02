package invoicing

import (
	"sort"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// Invoice.status and Receipt.status are set by this module's own lifecycle
// code, never by a generic CRUD route — but they are still enum fields, and an
// enum whose declared set disagrees with the constants the code writes would
// make the module unable to save its own documents.
func TestDocumentStatusOptionsMatchModuleConstants(t *testing.T) {
	want := []string{StatusDraft, StatusPosted}
	sort.Strings(want)

	for name, yaml := range map[string]string{
		"Invoice": invoiceYAML,
		"Receipt": receiptYAML,
	} {
		t.Run(name, func(t *testing.T) {
			eff, err := effective(yaml)
			if err != nil {
				t.Fatalf("effective: %v", err)
			}
			var got []string
			for _, f := range eff.Fields {
				if f.Name == "status" {
					if f.Type != metadata.FieldEnum {
						t.Fatalf("%s.status is %q, not an enum", name, f.Type)
					}
					got = append(got, f.Options...)
				}
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("%s.status options = %v, want %v", name, got, want)
			}
		})
	}
}

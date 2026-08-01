package ledger

import (
	"sort"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// The enum option sets in the YAML and the module's Go constants are two
// hand-written copies of one closed set. These tests are what stops them
// drifting: the alternative — interpolating the constants into the YAML —
// would make the schema unreadable to save a test that costs nothing.

func optionsFor(t *testing.T, schema *metadata.EffectiveSchema, field string) []string {
	t.Helper()
	for _, f := range schema.Fields {
		if f.Name == field {
			if f.Type != metadata.FieldEnum {
				t.Fatalf("field %q is %q, not an enum", field, f.Type)
			}
			out := append([]string(nil), f.Options...)
			sort.Strings(out)
			return out
		}
	}
	t.Fatalf("field %q not found on %s", field, schema.ObjectName)
	return nil
}

func TestAccountTypeOptionsMatchModuleConstants(t *testing.T) {
	schema, err := AccountSchema()
	if err != nil {
		t.Fatalf("AccountSchema: %v", err)
	}
	want := []string{AccountAsset, AccountEquity, AccountExpense, AccountIncome, AccountLiability}
	sort.Strings(want)
	if got := optionsFor(t, schema, "type"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Account.type options = %v, want %v — the schema and the module constants have drifted", got, want)
	}
}

func TestPeriodStatusOptionsMatchModuleConstants(t *testing.T) {
	eff, err := effective(periodYAML)
	if err != nil {
		t.Fatalf("effective(periodYAML): %v", err)
	}
	want := []string{PeriodClosed, PeriodOpen}
	sort.Strings(want)
	if got := optionsFor(t, eff, "status"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Period.status options = %v, want %v", got, want)
	}
}

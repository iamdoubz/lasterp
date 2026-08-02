package metadata

import (
	"errors"
	"strings"
	"testing"
)

// narrowCore is a core object with one enum field, for the option-ceiling tests.
const narrowCoreYAML = `
object: Account
module: ledger
persistence: crud
fields:
  - {name: code, type: text, required: true}
  - {name: type, type: enum, required: true, options: [asset, liability, equity, income, expense]}
permissions:
  read: [ledger.viewer]
  create: [ledger.admin]
`

func narrowCore(t *testing.T) *Object {
	t.Helper()
	o, err := ParseObject([]byte(narrowCoreYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	return o
}

func optionsOf(t *testing.T, eff *EffectiveSchema, field string) string {
	t.Helper()
	for _, f := range eff.Fields {
		if f.Name == field {
			return strings.Join(f.Options, ",")
		}
	}
	t.Fatalf("field %q not found", field)
	return ""
}

// TestOverlayNarrowsOptionSet is the accepted half of AC 3.
func TestOverlayNarrowsOptionSet(t *testing.T) {
	core := narrowCore(t)
	eff, err := Merge(core, Overlay{
		Layer:         "tenant",
		NarrowOptions: map[string][]string{"type": {"asset", "expense"}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := optionsOf(t, eff, "type"); got != "asset,expense" {
		t.Fatalf("options = %q, want asset,expense", got)
	}
	// core must not be mutated — every other tenant merges from the same
	// pointer and would otherwise inherit this tenant's restriction.
	if got := strings.Join(core.Fields[1].Options, ","); got != "asset,liability,equity,income,expense" {
		t.Fatalf("Merge mutated the core object: %q", got)
	}
}

// TestOverlayWideningOptionSetIsRefused is the refused half of AC 3, and the
// data-side expression of INV-T3: a tenant admitting a value core never
// declared is doing what granting itself a withheld role would do to
// permissions.
func TestOverlayWideningOptionSetIsRefused(t *testing.T) {
	_, err := Merge(narrowCore(t), Overlay{
		Layer:         "tenant",
		NarrowOptions: map[string][]string{"type": {"asset", "banana"}},
	})
	if !errors.Is(err, ErrOptionSetWidened) {
		t.Fatalf("Merge = %v, want ErrOptionSetWidened", err)
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error %q should name the offending value", err)
	}
}

// TestNarrowingComposesAcrossLayers: each layer may narrow further, and no
// layer can restore what an earlier one removed. That monotonicity is what
// makes the ceiling meaningful with plugin and tenant overlays stacked.
func TestNarrowingComposesAcrossLayers(t *testing.T) {
	core := narrowCore(t)

	eff, err := Merge(core,
		Overlay{Layer: "plugin", NarrowOptions: map[string][]string{"type": {"asset", "liability", "expense"}}},
		Overlay{Layer: "tenant", NarrowOptions: map[string][]string{"type": {"asset"}}},
	)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := optionsOf(t, eff, "type"); got != "asset" {
		t.Fatalf("options = %q, want asset", got)
	}

	// A later layer trying to restore a value the earlier one dropped is a
	// widening, even though the value is in the core set.
	_, err = Merge(core,
		Overlay{Layer: "plugin", NarrowOptions: map[string][]string{"type": {"asset"}}},
		Overlay{Layer: "tenant", NarrowOptions: map[string][]string{"type": {"asset", "income"}}},
	)
	if !errors.Is(err, ErrOptionSetWidened) {
		t.Fatalf("restoring a dropped option = %v, want ErrOptionSetWidened", err)
	}
}

func TestNarrowUnknownOrNonEnumFieldIsRefused(t *testing.T) {
	for name, narrowing := range map[string]map[string][]string{
		"unknown field": {"nope": {"a"}},
		"non-enum":      {"code": {"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Merge(narrowCore(t), Overlay{Layer: "tenant", NarrowOptions: narrowing})
			if !errors.Is(err, ErrOverlayConflict) {
				t.Fatalf("Merge = %v, want ErrOverlayConflict", err)
			}
		})
	}
}

// TestNarrowToEmptySetIsRefused: an option set of nothing makes a required
// enum unwritable, which is a broken tenant rather than a customization.
func TestNarrowToEmptySetIsRefused(t *testing.T) {
	_, err := Merge(narrowCore(t), Overlay{
		Layer:         "tenant",
		NarrowOptions: map[string][]string{"type": {}},
	})
	if !errors.Is(err, ErrOverlayConflict) {
		t.Fatalf("Merge = %v, want ErrOverlayConflict", err)
	}
}

// TestOverlayAddedEnumDeclaresItsOwnOptions: an overlay's new enum field goes
// through the same Field.validate as a core one, so it cannot arrive
// unconstrained — and its options become the ceiling for later layers.
func TestOverlayAddedEnumDeclaresItsOwnOptions(t *testing.T) {
	_, err := Merge(narrowCore(t), Overlay{
		Layer:     "tenant",
		AddFields: []Field{{Name: "tier", Type: FieldEnum}},
	})
	if !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("Merge = %v, want ErrInvalidObject (overlay enum with no options)", err)
	}

	eff, err := Merge(narrowCore(t),
		Overlay{Layer: "plugin", AddFields: []Field{{Name: "tier", Type: FieldEnum, Options: []string{"gold", "silver"}}}},
		Overlay{Layer: "tenant", NarrowOptions: map[string][]string{"tier": {"gold"}}},
	)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := optionsOf(t, eff, "tier"); got != "gold" {
		t.Fatalf("options = %q, want gold", got)
	}
}

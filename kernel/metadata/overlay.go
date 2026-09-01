// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"errors"
	"fmt"
	"sort"
)

// Overlay is a single customization layer (module, plugin, or tenant —
// ADR-006). WP-0.5 supports exactly the two operations that change
// storage or authorization shape: adding fields, and additive permission
// changes (decision 5) — relabel/hide/UI-layout/workflow overlay
// operations have no bearing on this WP's AC and are out of scope.
type Overlay struct {
	// Object is the shipped object this overlay targets. It lives *in* the
	// document rather than in a filename or a route parameter so an exported
	// overlay is self-describing — ADR-006's "customization packages,
	// versionable in git" — and so a plugin bundle cannot retarget an approved
	// overlay by renaming a file (WP-3.2c-decisions.md §5).
	Object string `yaml:"object"`

	// Layer is the ADR-006 layer this overlay belongs to. Set by whatever
	// loaded it (the store stamps the column, a module stamps its own name);
	// never read from the document, because a document that names its own
	// position in the stack is a document that can lie about it.
	Layer string `yaml:"-"`

	AddFields   []Field     `yaml:"add_fields"`
	Permissions Permissions `yaml:"permissions"`

	// NarrowOptions restricts an existing enum field's option set to a subset
	// of what the layers before it declared: field name → the allowed values.
	//
	// It is a distinct verb rather than a re-declaration of the field because
	// AddFields is add-only by design (a name collision is ErrOverlayConflict),
	// and making a collision mean "redefine" is the in-place metadata mutation
	// ADR-006 explicitly rejects. There is no shape of this map that widens.
	NarrowOptions map[string][]string `yaml:"narrow_options"`
}

// ErrOverlayConflict is returned when an overlay's new field collides
// with an already-defined field name (core or an earlier-merged overlay).
var ErrOverlayConflict = errors.New("metadata: overlay conflict")

// ErrPermissionFloorLowered is returned when an overlay's permission
// entry for an action is not a superset of what an earlier layer already
// required — ADR-006: overlays "may not ... weaken core invariants
// (double-entry, permission floors)".
var ErrPermissionFloorLowered = errors.New("metadata: overlay would lower a permission floor")

// ErrOptionSetWidened is returned when an overlay's NarrowOptions entry is not
// a subset of the option set the layers before it declared.
//
// It is the mirror image of ErrPermissionFloorLowered, and the symmetry is the
// point. Merge already holds one bound on overlays: Permissions must be a
// *superset* of what an earlier layer required. Options must stay *within* the
// core set. Both express the same constitutional rule (INV-T3) —
//
//	the core layer's declaration is a bound no later layer may escape
//
// — and they bound from opposite sides only because the two things being
// declared point in opposite directions. Permissions are a floor: a role is a
// capability, and removing one takes away access core promised. Options are a
// ceiling: a value is a domain, and adding one admits data core never declared.
// A tenant that widens Account.type to admit "banana" is doing the data
// equivalent of granting itself a role core withheld.
var ErrOptionSetWidened = errors.New("metadata: overlay would widen an option set")

// EffectiveSchema is a core Object with every overlay merged in.
type EffectiveSchema struct {
	Object
}

// Merge folds overlays onto core in order, producing the effective
// schema the DDL generator and CRUD engine operate on. core is never
// mutated.
func Merge(core *Object, overlays ...Overlay) (*EffectiveSchema, error) {
	if err := core.Validate(); err != nil {
		return nil, err
	}

	eff := &EffectiveSchema{Object: *core}
	eff.Fields = append([]Field(nil), core.Fields...)
	eff.Permissions = clonePermissions(core.Permissions)

	fieldNames := make(map[string]bool, len(eff.Fields))
	for _, f := range eff.Fields {
		fieldNames[f.Name] = true
	}

	for _, ov := range overlays {
		for _, f := range ov.AddFields {
			if fieldNames[f.Name] {
				return nil, fmt.Errorf("%w: field %q already defined", ErrOverlayConflict, f.Name)
			}
			if err := f.validate(); err != nil {
				return nil, err
			}
			f.FromOverlay = true
			fieldNames[f.Name] = true
			eff.Fields = append(eff.Fields, f)
		}

		for action, roles := range ov.Permissions {
			existing := eff.Permissions[action]
			if !isSuperset(roles, existing) {
				return nil, fmt.Errorf("%w: action %q would drop %v", ErrPermissionFloorLowered, action, missing(existing, roles))
			}
			eff.Permissions[action] = append([]string(nil), roles...)
		}

		if err := narrow(eff, ov.NarrowOptions); err != nil {
			return nil, err
		}
	}
	return eff, nil
}

// narrow applies one overlay's option restrictions to eff in place. Field names
// are visited in sorted order so a malformed overlay always reports the same
// field first — map iteration order is otherwise a source of flaky messages.
func narrow(eff *EffectiveSchema, narrowings map[string][]string) error {
	names := make([]string, 0, len(narrowings))
	for name := range narrowings {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		options := narrowings[name]
		i := fieldIndex(eff.Fields, name)
		if i < 0 {
			return fmt.Errorf("%w: cannot narrow unknown field %q", ErrOverlayConflict, name)
		}
		if eff.Fields[i].Type != FieldEnum {
			return fmt.Errorf("%w: cannot narrow field %q of type %q (only enum has an option set)",
				ErrOverlayConflict, name, eff.Fields[i].Type)
		}
		// Narrowing to nothing makes a required enum unwritable. That is a
		// broken tenant, not a customization.
		if len(options) == 0 {
			return fmt.Errorf("%w: narrowing field %q to an empty option set would make it unwritable",
				ErrOverlayConflict, name)
		}
		if added := missing(options, eff.Fields[i].Options); len(added) > 0 {
			return fmt.Errorf("%w: field %q would gain %v (an overlay may narrow an option set, never widen it)",
				ErrOptionSetWidened, name, added)
		}
		// Reassigning the whole slice (rather than filtering in place) is what
		// makes narrowing compose monotonically: the next layer sees only what
		// this one left, so a value an earlier layer removed can never return.
		eff.Fields[i].Options = append([]string(nil), options...)
	}
	return nil
}

func fieldIndex(fields []Field, name string) int {
	for i, f := range fields {
		if f.Name == name {
			return i
		}
	}
	return -1
}

func clonePermissions(p Permissions) Permissions {
	out := make(Permissions, len(p))
	for action, roles := range p {
		out[action] = append([]string(nil), roles...)
	}
	return out
}

func isSuperset(set, subset []string) bool {
	return len(missing(subset, set)) == 0
}

// missing returns the elements of "of" not present in "in".
func missing(of, in []string) []string {
	present := make(map[string]bool, len(in))
	for _, s := range in {
		present[s] = true
	}
	var out []string
	for _, s := range of {
		if !present[s] {
			out = append(out, s)
		}
	}
	return out
}

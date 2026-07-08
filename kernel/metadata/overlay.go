// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"errors"
	"fmt"
)

// Overlay is a single customization layer (module, plugin, or tenant —
// ADR-006). WP-0.5 supports exactly the two operations that change
// storage or authorization shape: adding fields, and additive permission
// changes (decision 5) — relabel/hide/UI-layout/workflow overlay
// operations have no bearing on this WP's AC and are out of scope.
type Overlay struct {
	Layer       string
	AddFields   []Field
	Permissions Permissions
}

// ErrOverlayConflict is returned when an overlay's new field collides
// with an already-defined field name (core or an earlier-merged overlay).
var ErrOverlayConflict = errors.New("metadata: overlay conflict")

// ErrPermissionFloorLowered is returned when an overlay's permission
// entry for an action is not a superset of what an earlier layer already
// required — ADR-006: overlays "may not ... weaken core invariants
// (double-entry, permission floors)".
var ErrPermissionFloorLowered = errors.New("metadata: overlay would lower a permission floor")

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
			if !validFieldTypes[f.Type] {
				return nil, fmt.Errorf("%w: field %q has unknown type %q", ErrInvalidObject, f.Name, f.Type)
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
	}
	return eff, nil
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

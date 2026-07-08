package metadata

import (
	"errors"
	"testing"
)

func sampleCore(t *testing.T) *Object {
	t.Helper()
	o, err := ParseObject([]byte(sampleContactYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	return o
}

func TestMergeAddsFieldsAndPermissions(t *testing.T) {
	core := sampleCore(t)
	overlay := Overlay{
		Layer:       "tenant",
		AddFields:   []Field{{Name: "vip", Type: FieldBool}},
		Permissions: Permissions{"read": {"crm.viewer", "crm.exec"}}, // superset: adds a role
	}

	eff, err := Merge(core, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(eff.Fields) != 4 {
		t.Fatalf("got %d fields, want 4 (3 core + 1 overlay)", len(eff.Fields))
	}
	found := false
	for _, f := range eff.Fields {
		if f.Name == "vip" && f.Type == FieldBool {
			found = true
		}
	}
	if !found {
		t.Fatal("overlay field \"vip\" not present in effective schema")
	}
	if len(eff.Permissions["read"]) != 2 {
		t.Fatalf("read permissions = %v, want 2 roles", eff.Permissions["read"])
	}

	// core must not be mutated by Merge.
	if len(core.Fields) != 3 {
		t.Fatalf("core.Fields mutated: got %d, want 3", len(core.Fields))
	}
}

func TestMergeFieldNameCollisionConflict(t *testing.T) {
	core := sampleCore(t)
	overlay := Overlay{AddFields: []Field{{Name: "email", Type: FieldText}}}
	if _, err := Merge(core, overlay); !errors.Is(err, ErrOverlayConflict) {
		t.Fatalf("err = %v, want ErrOverlayConflict", err)
	}
}

func TestMergeTwoOverlaysCollideOnNewField(t *testing.T) {
	core := sampleCore(t)
	first := Overlay{Layer: "module", AddFields: []Field{{Name: "vip", Type: FieldBool}}}
	second := Overlay{Layer: "tenant", AddFields: []Field{{Name: "vip", Type: FieldText}}}
	if _, err := Merge(core, first, second); !errors.Is(err, ErrOverlayConflict) {
		t.Fatalf("err = %v, want ErrOverlayConflict", err)
	}
}

// INV-T3-flavored: an overlay may not lower a permission floor (ADR-006).
func TestMergePermissionFloorLoweredConflict(t *testing.T) {
	core := sampleCore(t)
	// core requires ["crm.admin"] for delete; overlay tries to replace it
	// with a different, non-superset role list.
	overlay := Overlay{Permissions: Permissions{"delete": {"crm.user"}}}
	if _, err := Merge(core, overlay); !errors.Is(err, ErrPermissionFloorLowered) {
		t.Fatalf("err = %v, want ErrPermissionFloorLowered", err)
	}
}

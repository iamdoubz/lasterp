package metadata

import (
	"errors"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// widgetObject builds a minimal CRUD object with the given fields for
// planner-only (no DB) diff tests.
func widgetObject(fields ...Field) *Object {
	return &Object{
		ObjectName:  "Widget",
		Module:      "test",
		Persistence: PersistenceCRUD,
		Fields:      fields,
		Permissions: Permissions{"read": {"r"}, "create": {"c"}, "update": {"u"}, "delete": {"d"}},
	}
}

func eff(t *testing.T, o *Object, overlays ...Overlay) *EffectiveSchema {
	t.Helper()
	e, err := Merge(o, overlays...)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return e
}

func TestPlanEvolutionAddOptionalField(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "name", Type: FieldText, Required: true}))
	v2 := eff(t, widgetObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "phone", Type: FieldText},
	))

	plan, err := PlanEvolution(v1, v2, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != AddColumn || plan.Steps[0].Field != "phone" {
		t.Fatalf("steps = %+v, want one AddColumn(phone)", plan.Steps)
	}
	for _, d := range []storage.Dialect{storage.Postgres, storage.SQLite} {
		if got := plan.DDL(d); !strings.Contains(got, "ADD COLUMN phone TEXT") {
			t.Fatalf("%v DDL = %q, want ADD COLUMN phone TEXT", d, got)
		}
	}
}

func TestPlanEvolutionAddIndexedField(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "name", Type: FieldText, Required: true}))
	v2 := eff(t, widgetObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "sku", Type: FieldText, Index: true},
	))

	plan, err := PlanEvolution(v1, v2, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Kind != AddColumn || plan.Steps[1].Kind != AddIndex {
		t.Fatalf("steps = %+v, want AddColumn then AddIndex", plan.Steps)
	}
	if got := plan.DDL(storage.Postgres); !strings.Contains(got, "CREATE INDEX idx_obj_widget_sku ON obj_widget (tenant_id, sku)") {
		t.Fatalf("DDL = %q, missing sku index", got)
	}
}

// INT → TEXT (int field widened to decimal): a real Postgres ALTER, a
// no-op on SQLite (type affinity round-trips the data).
func TestPlanEvolutionWidenType(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "points", Type: FieldInt}))
	v2 := eff(t, widgetObject(Field{Name: "points", Type: FieldDecimal}))

	plan, err := PlanEvolution(v1, v2, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != WidenColumn {
		t.Fatalf("steps = %+v, want one WidenColumn", plan.Steps)
	}
	if got := plan.DDL(storage.Postgres); !strings.Contains(got, "ALTER COLUMN points TYPE TEXT USING (points::TEXT)") {
		t.Fatalf("Postgres DDL = %q, want ALTER COLUMN ... TYPE TEXT USING", got)
	}
	if got := plan.DDL(storage.SQLite); got != "" {
		t.Fatalf("SQLite DDL = %q, want empty (affinity no-op)", got)
	}
}

func TestPlanEvolutionLoosenRequired(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "phone", Type: FieldText, Required: true}))
	v2 := eff(t, widgetObject(Field{Name: "phone", Type: FieldText}))

	plan, err := PlanEvolution(v1, v2, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != DropNotNull {
		t.Fatalf("steps = %+v, want one DropNotNull", plan.Steps)
	}
	if got := plan.DDL(storage.Postgres); !strings.Contains(got, "ALTER COLUMN phone DROP NOT NULL") {
		t.Fatalf("Postgres DDL = %q, want DROP NOT NULL", got)
	}
	if got := plan.DDL(storage.SQLite); got != "" {
		t.Fatalf("SQLite DDL = %q, want empty", got)
	}
}

// Overlay fields live in custom_fields — adding one is not a DDL change.
func TestPlanEvolutionOverlayFieldIsNoOp(t *testing.T) {
	base := widgetObject(Field{Name: "name", Type: FieldText, Required: true})
	v1 := eff(t, base)
	v2 := eff(t, base, Overlay{Layer: "tenant", AddFields: []Field{{Name: "vip", Type: FieldBool}}})

	plan, err := PlanEvolution(v1, v2, 1, 2)
	if err != nil {
		t.Fatalf("PlanEvolution: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("steps = %+v, want none (overlay field is custom_fields, no DDL)", plan.Steps)
	}
	if got := plan.DDL(storage.Postgres); got != "" {
		t.Fatalf("DDL = %q, want empty", got)
	}
}

func TestPlanEvolutionRejectsDropField(t *testing.T) {
	v1 := eff(t, widgetObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "email", Type: FieldEmail},
	))
	v2 := eff(t, widgetObject(Field{Name: "name", Type: FieldText, Required: true}))

	_, err := PlanEvolution(v1, v2, 1, 2)
	if !errors.Is(err, ErrDestructiveDiff) || !strings.Contains(err.Error(), "email") {
		t.Fatalf("err = %v, want ErrDestructiveDiff naming email", err)
	}
}

func TestPlanEvolutionRejectsNarrowing(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "code", Type: FieldText}))
	v2 := eff(t, widgetObject(Field{Name: "code", Type: FieldInt}))

	_, err := PlanEvolution(v1, v2, 1, 2)
	if !errors.Is(err, ErrDestructiveDiff) || !strings.Contains(err.Error(), "TEXT→INT") {
		t.Fatalf("err = %v, want ErrDestructiveDiff for TEXT→INT", err)
	}
}

func TestPlanEvolutionRejectsNewRequiredField(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "name", Type: FieldText, Required: true}))
	v2 := eff(t, widgetObject(
		Field{Name: "name", Type: FieldText, Required: true},
		Field{Name: "tax_id", Type: FieldText, Required: true},
	))

	_, err := PlanEvolution(v1, v2, 1, 2)
	if !errors.Is(err, ErrDestructiveDiff) || !strings.Contains(err.Error(), "tax_id") {
		t.Fatalf("err = %v, want ErrDestructiveDiff naming tax_id", err)
	}
}

func TestPlanEvolutionRejectsTighteningRequired(t *testing.T) {
	v1 := eff(t, widgetObject(Field{Name: "phone", Type: FieldText}))
	v2 := eff(t, widgetObject(Field{Name: "phone", Type: FieldText, Required: true}))

	_, err := PlanEvolution(v1, v2, 1, 2)
	if !errors.Is(err, ErrDestructiveDiff) || !strings.Contains(err.Error(), "becomes required") {
		t.Fatalf("err = %v, want ErrDestructiveDiff for tightening required", err)
	}
}

func TestPlanEvolutionRejectsNonMonotonic(t *testing.T) {
	v := eff(t, widgetObject(Field{Name: "name", Type: FieldText, Required: true}))
	if _, err := PlanEvolution(v, v, 2, 2); !errors.Is(err, ErrNonMonotonicVersion) {
		t.Fatalf("err = %v, want ErrNonMonotonicVersion for to==from", err)
	}
	if _, err := PlanEvolution(v, v, 3, 2); !errors.Is(err, ErrNonMonotonicVersion) {
		t.Fatalf("err = %v, want ErrNonMonotonicVersion for to<from", err)
	}
}

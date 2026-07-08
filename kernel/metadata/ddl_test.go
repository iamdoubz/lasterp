package metadata

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

func TestApplyDDLCreatesQueryableTable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			core := sampleCore(t)
			eff, err := Merge(core)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}

			if err := ApplyDDL(ctx, db, eff, 1); err != nil {
				t.Fatalf("ApplyDDL: %v", err)
			}

			var n int
			row := db.QueryRowContext(ctx, db.Rebind(`SELECT COUNT(*) FROM `+TableName(eff.ObjectName)))
			if err := row.Scan(&n); err != nil {
				t.Fatalf("query generated table: %v", err)
			}
			if n != 0 {
				t.Fatalf("count = %d, want 0 on a freshly created table", n)
			}
		})
	}
}

// ApplyDDL is idempotent: re-applying the same version doesn't try (and
// fail) to CREATE TABLE again.
func TestApplyDDLIsIdempotent(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			core := sampleCore(t)
			eff, err := Merge(core)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}

			if err := ApplyDDL(ctx, db, eff, 1); err != nil {
				t.Fatalf("first ApplyDDL: %v", err)
			}
			if err := ApplyDDL(ctx, db, eff, 1); err != nil {
				t.Fatalf("second ApplyDDL (should be a no-op): %v", err)
			}
		})
	}
}

func TestGenerateDDLRejectsTableFieldType(t *testing.T) {
	core := sampleCore(t)
	core.Fields = append(core.Fields, Field{Name: "lines", Type: FieldTable})
	eff, err := Merge(core)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_, err = GenerateDDL(eff, storage.Postgres)
	if !errors.Is(err, ErrUnsupportedFieldType) {
		t.Fatalf("err = %v, want ErrUnsupportedFieldType", err)
	}
}

func TestTableNameIsDeterministic(t *testing.T) {
	if got, want := TableName("Contact"), "obj_contact"; got != want {
		t.Fatalf("TableName(%q) = %q, want %q", "Contact", got, want)
	}
}

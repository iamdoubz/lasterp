package metadata

import (
	"context"
	"errors"
	"testing"
)

func TestSaveAndLoadObjectSchema(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			def := []byte(sampleContactYAML)
			if err := SaveObjectSchema(ctx, db, tenant, LayerTenant, "Contact", 1, def); err != nil {
				t.Fatalf("SaveObjectSchema: %v", err)
			}

			got, err := LoadObjectSchema(ctx, db, tenant, LayerTenant, "Contact")
			if err != nil {
				t.Fatalf("LoadObjectSchema: %v", err)
			}
			if string(got.Definition) != string(def) {
				t.Fatalf("Definition = %q, want %q", got.Definition, def)
			}
			if got.Checksum == "" {
				t.Fatal("Checksum is empty")
			}
			if got.Version != 1 {
				t.Fatalf("Version = %d, want 1", got.Version)
			}
		})
	}
}

func TestLoadObjectSchemaNotFound(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			if _, err := LoadObjectSchema(ctx, db, tenant, LayerTenant, "NoSuchObject"); !errors.Is(err, ErrSchemaNotFound) {
				t.Fatalf("err = %v, want ErrSchemaNotFound", err)
			}
		})
	}
}

// Core-layer schemas are shared across every tenant (stored under a
// sentinel tenant), not tenant-scoped.
func TestCoreLayerSchemaVisibleAcrossTenants(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			def := []byte(sampleContactYAML)
			if err := SaveObjectSchema(ctx, db, tenantA, LayerCore, "Contact", 1, def); err != nil {
				t.Fatalf("SaveObjectSchema (core): %v", err)
			}

			got, err := LoadObjectSchema(ctx, db, tenantB, LayerCore, "Contact")
			if err != nil {
				t.Fatalf("LoadObjectSchema from a different tenant: %v", err)
			}
			if string(got.Definition) != string(def) {
				t.Fatalf("Definition = %q, want %q", got.Definition, def)
			}
		})
	}
}

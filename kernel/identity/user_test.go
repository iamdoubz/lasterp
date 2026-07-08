package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func mustCreateTenant(t *testing.T, db *storage.DB) tenancy.ID {
	t.Helper()
	id := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, id, "test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

func TestCreateAndGetUser(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			hash, err := HashPassword("s3cret!")
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
			u, err := CreateUser(ctx, db, tenant, "alice@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			got, err := GetUserByEmail(ctx, db, tenant, "alice@example.com")
			if err != nil {
				t.Fatalf("GetUserByEmail: %v", err)
			}
			if got.ID != u.ID {
				t.Fatalf("got user id %s, want %s", got.ID, u.ID)
			}

			if _, err := GetUserByID(ctx, db, tenant, "no-such-id"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetUserByID unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestCreateUserRequiresTenantAndEmail(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			if _, err := CreateUser(ctx, db, "", "a@example.com", "hash"); err == nil {
				t.Fatal("CreateUser with empty tenant succeeded, want error")
			}
			tenant := mustCreateTenant(t, db)
			if _, err := CreateUser(ctx, db, tenant, "", "hash"); err == nil {
				t.Fatal("CreateUser with empty email succeeded, want error")
			}
		})
	}
}

// INV-T1: a user created under one tenant is invisible to a lookup scoped
// to a different tenant, even by exact email match.
func TestGetUserByEmailCrossTenantIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			hash, _ := HashPassword("s3cret!")
			if _, err := CreateUser(ctx, db, tenantA, "shared@example.com", hash); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			if _, err := GetUserByEmail(ctx, db, tenantB, "shared@example.com"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-tenant lookup: err = %v, want ErrNotFound", err)
			}
		})
	}
}

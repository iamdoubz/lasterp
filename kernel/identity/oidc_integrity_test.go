//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"errors"
	"testing"
)

// INV-T1 — "no query path returns another tenant's rows". WP-1.9 adds a login
// path keyed on an external identity rather than on anything the tenant owns,
// and one corporate IdP legitimately backs several tenants in a deployment. If
// the subject lookup were not tenant-scoped, an employee with an account in one
// tenant would authenticate into every other tenant backed by the same IdP —
// the RLS policy on users is the backstop, and this proves both halves hold.

func TestOIDCSubjectDoesNotCrossTenants(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")

			// The same person, by email, exists in both tenants — the
			// multi-tenant model gives them one users row each (WP-0.3 §6).
			alice, err := CreateUser(ctx, db, tenantA, "alice@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser in A: %v", err)
			}
			if _, err := CreateUser(ctx, db, tenantB, "alice@example.com", hash); err != nil {
				t.Fatalf("CreateUser in B: %v", err)
			}

			// Link the subject in tenant A only.
			if err := LinkOIDCIdentity(ctx, db, tenantA, alice.ID, testIssuer, "subject-1"); err != nil {
				t.Fatalf("link in A: %v", err)
			}

			// The lookup must not see A's row from B's context, and must not
			// resolve to A's user id under any circumstance.
			if _, err := GetUserByOIDCSubject(ctx, db, tenantB, testIssuer, "subject-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetUserByOIDCSubject in B found A's binding: err = %v, want ErrNotFound", err)
			}

			got, err := ResolveOIDCUser(ctx, db, tenantA, testClaims("subject-1", "alice@example.com", true))
			if err != nil {
				t.Fatalf("resolve in A: %v", err)
			}
			if got != alice.ID {
				t.Fatalf("resolve in A returned %s, want %s", got, alice.ID)
			}

			// Resolving in B links B's own user — a separate principal that
			// happens to share an external identity. It must never be A's.
			gotB, err := ResolveOIDCUser(ctx, db, tenantB, testClaims("subject-1", "alice@example.com", true))
			if err != nil {
				t.Fatalf("resolve in B: %v", err)
			}
			if gotB == alice.ID {
				t.Fatal("an OIDC login in tenant B resolved to tenant A's user — cross-tenant authentication (INV-T1)")
			}

			// And the two bindings coexist: the unique index is per tenant, so
			// one IdP subject legitimately maps to one user in each tenant.
			if _, err := GetUserByOIDCSubject(ctx, db, tenantA, testIssuer, "subject-1"); err != nil {
				t.Errorf("A's binding broke when B linked the same subject: %v", err)
			}
		})
	}
}

// TestOIDCLinkDoesNotCrossTenants: an attempt to link a user id from another
// tenant must write nothing, even though the id is a valid users row.
func TestOIDCLinkDoesNotCrossTenants(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")

			alice, err := CreateUser(ctx, db, tenantA, "alice@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser in A: %v", err)
			}

			// Tenant B's context, tenant A's user id. The UPDATE is filtered on
			// tenant_id and (on Postgres) additionally fenced by RLS, so it
			// must match no rows rather than silently binding A's user.
			if err := LinkOIDCIdentity(ctx, db, tenantB, alice.ID, testIssuer, "subject-9"); !errors.Is(err, ErrAlreadyLinked) {
				t.Fatalf("cross-tenant link: err = %v, want no rows affected (ErrAlreadyLinked)", err)
			}
			if _, err := GetUserByOIDCSubject(ctx, db, tenantA, testIssuer, "subject-9"); !errors.Is(err, ErrNotFound) {
				t.Errorf("a cross-tenant link wrote to tenant A: err = %v, want ErrNotFound (INV-T1)", err)
			}
		})
	}
}

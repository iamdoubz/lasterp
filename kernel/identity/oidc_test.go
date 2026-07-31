// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/oidc"
)

const testIssuer = "https://idp.example.com/realms/acme"

func testClaims(subject, email string, verified bool) *oidc.Claims {
	return &oidc.Claims{
		Issuer:        testIssuer,
		Subject:       subject,
		Email:         email,
		EmailVerified: verified,
	}
}

// TestResolveOIDCUserLinksOnFirstUseThenMatchesBySubject is the steady-state
// path: an administrator created the user, the first SSO login binds the
// subject to it, and every later login resolves without touching email at all.
func TestResolveOIDCUserLinksOnFirstUseThenMatchesBySubject(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "grace@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			claims := testClaims("subject-1", "grace@example.com", true)
			got, err := ResolveOIDCUser(ctx, db, tenant, claims)
			if err != nil {
				t.Fatalf("first login: %v", err)
			}
			if got != u.ID {
				t.Fatalf("first login resolved %s, want %s", got, u.ID)
			}

			// The binding is persisted, so a second login no longer depends on
			// the email claim — proven by changing it.
			renamed := testClaims("subject-1", "grace.hopper@example.com", true)
			got, err = ResolveOIDCUser(ctx, db, tenant, renamed)
			if err != nil {
				t.Fatalf("second login after an email change at the IdP: %v", err)
			}
			if got != u.ID {
				t.Fatalf("second login resolved %s, want %s", got, u.ID)
			}
		})
	}
}

func TestResolveOIDCUserRefusals(t *testing.T) {
	tests := []struct {
		name string
		// seedEmail, when set, creates a local user with that address.
		seedEmail string
		claims    *oidc.Claims
	}{
		{
			// No JIT provisioning: an IdP cannot make a principal appear in a
			// tenant (WP-1.9-decisions.md §3).
			name:   "no local user with that email",
			claims: testClaims("subject-1", "stranger@example.com", true),
		},
		{
			// The account-takeover guard. An IdP that lets a user type an
			// arbitrary address would otherwise hand them any local account.
			name:      "email is not verified by the IdP",
			seedEmail: "heidi@example.com",
			claims:    testClaims("subject-1", "heidi@example.com", false),
		},
		{
			name:      "no email claim at all",
			seedEmail: "heidi@example.com",
			claims:    testClaims("subject-1", "", true),
		},
		{
			name:      "no subject",
			seedEmail: "heidi@example.com",
			claims:    testClaims("", "heidi@example.com", true),
		},
		{
			name:      "no issuer",
			seedEmail: "heidi@example.com",
			claims:    &oidc.Claims{Subject: "subject-1", Email: "heidi@example.com", EmailVerified: true},
		},
		{
			name:   "no claims at all",
			claims: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for dialect, db := range testDialects(t) {
				t.Run(dialect, func(t *testing.T) {
					ctx := context.Background()
					tenant := mustCreateTenant(t, db)
					if tc.seedEmail != "" {
						hash, _ := HashPassword("s3cret!")
						if _, err := CreateUser(ctx, db, tenant, tc.seedEmail, hash); err != nil {
							t.Fatalf("CreateUser: %v", err)
						}
					}
					if _, err := ResolveOIDCUser(ctx, db, tenant, tc.claims); !errors.Is(err, ErrInvalidCredentials) {
						t.Errorf("err = %v, want ErrInvalidCredentials", err)
					}
				})
			}
		})
	}
}

// TestLinkOIDCIdentityIsWriteOnce: once a user is bound to a subject, a second
// subject cannot claim it by presenting the same verified email. Without this,
// renaming a directory account would be an account-takeover primitive.
func TestLinkOIDCIdentityIsWriteOnce(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "ivan@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if err := LinkOIDCIdentity(ctx, db, tenant, u.ID, testIssuer, "subject-1"); err != nil {
				t.Fatalf("first link: %v", err)
			}
			if err := LinkOIDCIdentity(ctx, db, tenant, u.ID, testIssuer, "subject-2"); !errors.Is(err, ErrAlreadyLinked) {
				t.Fatalf("relink: err = %v, want ErrAlreadyLinked", err)
			}

			// And the resolution path refuses rather than surfacing the
			// internal error.
			if _, err := ResolveOIDCUser(ctx, db, tenant, testClaims("subject-2", "ivan@example.com", true)); !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("resolve with a second subject: err = %v, want ErrInvalidCredentials", err)
			}
			// The original binding still works.
			got, err := ResolveOIDCUser(ctx, db, tenant, testClaims("subject-1", "ivan@example.com", true))
			if err != nil || got != u.ID {
				t.Errorf("original subject resolved (%s, %v), want (%s, nil)", got, err, u.ID)
			}
		})
	}
}

// TestGetUserByOIDCSubjectRequiresEveryPart guards the lookup against being
// satisfied by NULL columns: every password-only user has NULL oidc_subject, so
// a query built with empty strings must find nobody rather than the first row
// with an unset binding.
func TestGetUserByOIDCSubjectRequiresEveryPart(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			if _, err := CreateUser(ctx, db, tenant, "judy@example.com", hash); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			for _, tc := range []struct{ issuer, subject string }{
				{"", ""},
				{testIssuer, ""},
				{"", "subject-1"},
			} {
				if _, err := GetUserByOIDCSubject(ctx, db, tenant, tc.issuer, tc.subject); !errors.Is(err, ErrNotFound) {
					t.Errorf("issuer=%q subject=%q: err = %v, want ErrNotFound", tc.issuer, tc.subject, err)
				}
			}
		})
	}
}

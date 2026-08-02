//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WP-1.10 AC: "exactly one code path issues a password session."
//
// Until WP-1.10 there were two. kernel/identity.PasswordTOTPProvider was the
// supposedly reusable implementation and nothing outside its own unit tests
// called it; internal/app's login handler reimplemented the same decision
// inline. They had already drifted — the handler had a dummy-bcrypt timing
// equalizer for unknown users and the provider did not — which is what two
// copies of an authentication decision do (phase-1-review.md P1 item 7).
//
// A behavioural test cannot catch the regression that matters here, because a
// second copy would pass every behavioural test by construction: it would be a
// *copy*. So this fence is structural. It is the same shape as
// kernel/integrity's registry gate, which greps the tree for invariant tags.
//
// Invariant: INV-T2 — no write path executes without an authenticated principal
// and an authorization decision. Session issuance is that decision's front door.

// passwordPrimitives are the low-level operations that, called outside
// kernel/identity, mean somebody is making an authentication decision somewhere
// they should not be.
var passwordPrimitives = []string{
	"VerifyPassword(",
	"ValidateTOTP(",
	// WP-1.12: a recovery code is a credential. Verifying one in the
	// composition root would be the same mistake the fence was built to catch,
	// one credential type later. internal/app calls Authenticate or
	// Reauthenticate; the provider is what spends the code.
	"ConsumeRecoveryCode(",
}

// TestOnlyOneCodePathAuthenticatesAPassword asserts that the composition root
// delegates authentication rather than performing it. internal/app may call
// AuthProvider.Authenticate; it may not compare a password or validate a TOTP
// code itself.
func TestOnlyOneCodePathAuthenticatesAPassword(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, primitive := range passwordPrimitives {
			if strings.Contains(string(src), primitive) {
				t.Errorf("%s calls identity.%s directly. Authentication decisions belong to an "+
					"identity.AuthProvider implementation — a second copy here is how the timing "+
					"equalizer went missing from one of them before WP-1.10 (INV-T2).", name, primitive)
			}
		}
	}
}

// TestLoginUsesTheProvider is the positive half: the login route must actually
// route through PasswordTOTPProvider, so the fence above cannot be satisfied by
// deleting authentication rather than delegating it.
func TestLoginUsesTheProvider(t *testing.T) {
	src, err := os.ReadFile("sessions.go")
	if err != nil {
		t.Fatalf("read sessions.go: %v", err)
	}
	if !strings.Contains(string(src), "identity.PasswordTOTPProvider{") {
		t.Error("sessions.go no longer constructs identity.PasswordTOTPProvider: the password login " +
			"path must go through the AuthProvider interface, which is the same seam OIDC uses (WP-1.9)")
	}
}

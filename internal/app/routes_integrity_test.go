//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/i18n"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
)

// WP-1.5 opened the first authentication bypass in the gateway: Action.Public,
// which session issuance cannot exist without (a login route has no session to
// present). These tests are the fence around it. INV-T2 — "no write path
// executes without an authenticated principal and authz decision" — now depends
// on a property no reviewer can eyeball across a growing route table, so it is
// asserted mechanically here instead.

// publicRouteAllowlist is the complete set of routes permitted to skip
// authentication. Adding an entry is a security decision: it must be a route
// that establishes authentication, never one that uses it. A new public route
// failing this test is the test doing its job — widen the allowlist only with
// the threat notes to match.
var publicRouteAllowlist = map[string]bool{
	"POST /api/v1/sessions":         true,
	"POST /api/v1/sessions/refresh": true,

	// WP-1.9 (OIDC). Both establish authentication rather than using it, which
	// is the bar for this list. The start route mints CSRF state and hands back
	// the provider's authorization URL; the callback is the provider's redirect
	// target and cannot present a session it has not yet issued. Neither can be
	// authenticated by construction, and the callback additionally cannot be
	// authenticated by protocol — the IdP redirects a browser to it.
	//
	// What keeps them honest: the callback refuses any request whose state does
	// not match the flow cookie it set, refuses an assertion that does not
	// resolve to an existing local user, and never provisions one
	// (oidc_integrity_test.go).
	"GET /api/v1/sessions/oidc":          true,
	"GET /api/v1/sessions/oidc/callback": true,
}

// TestPublicRoutesAreExactlyTheAllowlist is the INV-T2 fence: every registered
// action is checked against the allowlist, and no public route may be a write or
// be capability-gated. A future WP that marks an ordinary endpoint Public fails
// CI here rather than shipping an unauthenticated hole.
func TestPublicRoutesAreExactlyTheAllowlist(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			for _, a := range allActions(t, db) {
				route := a.Method + " " + a.Path
				if !a.Public {
					if publicRouteAllowlist[route] {
						t.Errorf("%s is in the public allowlist but is not registered Public", route)
					}
					continue
				}
				if !publicRouteAllowlist[route] {
					t.Errorf("%s is Public but not in the allowlist — unauthenticated routes are a deliberate, reviewed exception (INV-T2)", route)
				}
				if a.Write {
					t.Errorf("%s is a public write: an unauthenticated mutation violates INV-T2", route)
				}
				if a.Object != "" {
					t.Errorf("%s is public and capability-gated on %q, but there is no tenant to gate on before authn", route, a.Object)
				}
			}
		})
	}
}

// TestEveryNonPublicRouteRejectsAnonymous proves the fence holds at runtime, not
// just in the route table: each registered action is called with no credential
// and must answer 401. This catches a route that bypasses the gateway choke
// point some other way — a handler registered directly on the mux, say.
func TestEveryNonPublicRouteRejectsAnonymous(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			for _, a := range allActions(t, db) {
				if a.Public {
					continue
				}
				route := a.Method + " " + a.Path
				// Path params are irrelevant: authn runs before routing reaches
				// the handler, so any concrete value exercises the same guard.
				path := concretePath(a.Path)
				// Send an Idempotency-Key on writes so a 400 for the missing
				// header can't be mistaken for the rejection we're asserting.
				idem := ""
				if a.Write {
					idem = idgen.New()
				}
				status, body, _ := e.call(a.Method, path, "", idem, map[string]any{})
				if status != http.StatusUnauthorized {
					t.Errorf("%s without credentials = %d, want 401; body=%s", route, status, body)
				}
			}
		})
	}
}

// TestPublicRoutesRejectBadCredentials confirms the two allowlisted routes are
// public but not permissive: reachable without a session, still refusing to
// issue one on bad input.
func TestPublicRoutesRejectBadCredentials(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			status, body, _ := e.call("POST", "/api/v1/sessions", "", "", map[string]any{
				"tenant": string(e.tenant), "email": "nobody@example.com", "password": "wrong",
			})
			if status != http.StatusUnauthorized {
				t.Errorf("login with unknown user = %d, want 401; body=%s", status, body)
			}

			// No refresh cookie at all.
			status, body, _ = e.call("POST", "/api/v1/sessions/refresh", "", "", nil)
			if status != http.StatusUnauthorized {
				t.Errorf("refresh without cookie = %d, want 401; body=%s", status, body)
			}
		})
	}
}

// allActions builds the real registered action table for db.
func allActions(t *testing.T, db *storage.DB) []api.Action {
	t.Helper()
	reg, err := capability.Load()
	if err != nil {
		t.Fatalf("capability.Load: %v", err)
	}
	objects, err := crudObjects()
	if err != nil {
		t.Fatalf("crudObjects: %v", err)
	}
	translator, err := i18n.Load()
	if err != nil {
		t.Fatalf("i18n.Load: %v", err)
	}
	// An IdP is configured on purpose: the OIDC routes are public, so they must
	// be enumerated by the fence. A fence that only sees them in deployments
	// that happen to enable SSO would let a future public route in unnoticed.
	idp := newFakeIdP(t)
	configureOIDC(t, idp, "route-fence-tenant")
	sso, err := newOIDCLogin(context.Background(), db)
	if err != nil {
		t.Fatalf("newOIDCLogin: %v", err)
	}
	if sso == nil {
		t.Fatal("newOIDCLogin returned nothing with an IdP configured")
	}
	return actions(db, reg, objects, translator, sso)
}

// concretePath substitutes a placeholder for each {param} segment.
func concretePath(pattern string) string {
	out := ""
	for _, seg := range splitPath(pattern) {
		if len(seg) > 1 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			seg = "x"
		}
		out += "/" + seg
	}
	return out
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

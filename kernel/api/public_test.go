// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// publicAction is a minimal public route that echoes back whether it ran.
func publicAction(t *testing.T, hit *bool) Action {
	t.Helper()
	return Action{
		Method: "POST", Path: "/api/v1/sessions", Public: true,
		Summary: "Log in",
		Handler: func(w http.ResponseWriter, _ *http.Request, tenant tenancy.ID) {
			*hit = true
			if tenant != "" {
				t.Errorf("public handler got tenant %q, want empty", tenant)
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		},
	}
}

// A public action is reachable with no Authenticator configured at all — the
// point of the seam (WP-1.5-decisions.md §4). Every other route stays closed.
func TestPublicActionSkipsAuthn(t *testing.T) {
	var hit bool
	g := NewGateway(Config{Actions: []Action{publicAction(t, &hit)}})

	rr := do(t, g, http.MethodPost, "/api/v1/sessions", "", map[string]any{"email": "a@b.c"})
	if rr.Code != http.StatusOK {
		t.Fatalf("public route status = %d, want 200; body=%s", rr.Code, rr.Body)
	}
	if !hit {
		t.Error("public handler did not run")
	}
}

// The escape hatch must not leak into authenticated routes: a non-public action
// on the same gateway still 401s without credentials (INV-T2).
func TestNonPublicActionStillRequiresAuthn(t *testing.T) {
	var hit bool
	closed := Action{
		Method: "GET", Path: "/api/v1/whoami",
		Summary: "Who am I",
		Handler: func(w http.ResponseWriter, _ *http.Request, _ tenancy.ID) {
			t.Error("authenticated handler ran without authn")
			writeJSON(w, http.StatusOK, nil)
		},
	}
	g := NewGateway(Config{Actions: []Action{publicAction(t, &hit), closed}})

	if rr := do(t, g, http.MethodGet, "/api/v1/whoami", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("authenticated route status = %d, want 401; body=%s", rr.Code, rr.Body)
	}
}

// Public + Write would be an unauthenticated mutation — exactly the INV-T2 hole
// the constraint exists to prevent. It must fail at boot, not at request time.
func TestPublicWriteActionPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("registering a public write action did not panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "INV-T2") {
			t.Errorf("panic = %v, want it to cite INV-T2", r)
		}
	}()
	NewGateway(Config{Actions: []Action{{
		Method: "POST", Path: "/api/v1/sessions", Public: true, Write: true,
		Handler: func(http.ResponseWriter, *http.Request, tenancy.ID) {},
	}}})
}

// A capability-gated public route has no tenant to gate on, so the gate would
// silently pass — reject the configuration instead.
func TestPublicGatedActionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a capability-gated public action did not panic")
		}
	}()
	NewGateway(Config{Actions: []Action{{
		Method: "GET", Path: "/api/v1/sessions", Public: true, Object: "Contact",
		Handler: func(http.ResponseWriter, *http.Request, tenancy.ID) {},
	}}})
}

// Public routes are rate-limited by client IP: one caller exhausting the login
// budget must not exhaust another's (a shared bucket would be a trivial
// lockout DoS against every unauthenticated user).
func TestPublicRouteRateLimitIsPerIP(t *testing.T) {
	var hit bool
	g := NewGateway(Config{
		Actions:   []Action{publicAction(t, &hit)},
		RateLimit: RateLimit{RequestsPerSecond: 1, Burst: 1},
	})

	post := func(remoteAddr string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader("{}"))
		r.RemoteAddr = remoteAddr
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, r)
		return rr.Code
	}

	if code := post("10.0.0.1:1111"); code != http.StatusOK {
		t.Fatalf("first request from IP A = %d, want 200", code)
	}
	if code := post("10.0.0.1:2222"); code != http.StatusTooManyRequests {
		t.Fatalf("second request from IP A = %d, want 429 (same IP, different port)", code)
	}
	if code := post("10.0.0.2:1111"); code != http.StatusOK {
		t.Fatalf("first request from IP B = %d, want 200 (separate bucket)", code)
	}
}

// X-Forwarded-For is caller-supplied; trusting it would hand an attacker an
// unlimited supply of fresh login budgets.
func TestPublicRateLimitIgnoresForwardedFor(t *testing.T) {
	var hit bool
	g := NewGateway(Config{
		Actions:   []Action{publicAction(t, &hit)},
		RateLimit: RateLimit{RequestsPerSecond: 1, Burst: 1},
	})

	post := func(xff string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader("{}"))
		r.RemoteAddr = "10.0.0.9:1234"
		r.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, r)
		return rr.Code
	}

	if code := post("1.1.1.1"); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := post("2.2.2.2"); code != http.StatusTooManyRequests {
		t.Fatalf("forged X-Forwarded-For got a fresh bucket (status %d), want 429", code)
	}
}

// A public route advertises no 401/403 (no credential is accepted) but does
// document its request body, so generated clients can call it.
func TestOpenAPIDocumentsPublicAction(t *testing.T) {
	var hit bool
	doc := OpenAPI(nil, []Action{publicAction(t, &hit)})
	paths, _ := doc["paths"].(obj)
	op, _ := paths["/api/v1/sessions"].(obj)["post"].(obj)
	if op == nil {
		t.Fatal("public action missing from OpenAPI paths")
	}
	resp, _ := op["responses"].(obj)
	if _, found := resp["401"]; found {
		t.Error("public action documents a 401; it accepts no credential")
	}
	if _, found := op["requestBody"]; !found {
		t.Error("public POST action documents no request body")
	}
	if params, found := op["parameters"]; found {
		t.Errorf("public action takes an Idempotency-Key parameter: %v", params)
	}
}

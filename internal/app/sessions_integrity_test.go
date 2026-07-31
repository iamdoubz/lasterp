//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// loginAs performs a real HTTP login and returns the response plus the cookie
// jar entries the server set, so tests can assert on transport as well as
// outcome.
func (e *env) loginAs(t *testing.T, email, password string) (*http.Response, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequest("POST", e.server.URL+"/api/v1/sessions", jsonBody(t, map[string]any{
		"tenant": string(e.tenant), "email": email, "password": password,
	}))
	if err != nil {
		t.Fatalf("new login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Don't follow redirects or reuse a jar — we want the raw Set-Cookie.
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, resp.Cookies()
}

// createLoginUser makes a user with a known password and full grants.
func (e *env) createLoginUser(t *testing.T, password string) (identity.UserID, string) {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	email := idgen.New() + "@example.com"
	user, err := identity.CreateUser(ctx, e.db, e.tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	grantRole(t, e.db, e.tenant, user.ID, fullGrants())
	return user.ID, email
}

// AC (auth): a browser can log in, and the credential it gets back is
// unreachable from JavaScript — HttpOnly, Secure, SameSite=Strict, and absent
// from the response body (WP-1.5-decisions.md §5).
func TestLoginSetsHttpOnlyCookiesAndNoBodyToken(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, email := e.createLoginUser(t, "c0rrect-horse")

			resp, cookies := e.loginAs(t, email, "c0rrect-horse")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("login status = %d, want 200", resp.StatusCode)
			}

			want := map[string]bool{cookieSession: false, cookieRefresh: false, cookieDevice: false}
			for _, c := range cookies {
				if _, tracked := want[c.Name]; !tracked {
					continue
				}
				want[c.Name] = true
				if !c.HttpOnly {
					t.Errorf("cookie %s is not HttpOnly: JS could read the session", c.Name)
				}
				if !c.Secure {
					t.Errorf("cookie %s is not Secure", c.Name)
				}
				if c.SameSite != http.SameSiteStrictMode {
					t.Errorf("cookie %s SameSite = %v, want Strict", c.Name, c.SameSite)
				}
				if c.Value == "" {
					t.Errorf("cookie %s is empty", c.Name)
				}
			}
			for cname, found := range want {
				if !found {
					t.Errorf("login did not set cookie %s", cname)
				}
			}

			var body map[string]any
			decodeBody(t, resp, &body)
			for _, leak := range []string{"token", "access_token", "refresh_token", "session"} {
				if _, found := body[leak]; found {
					t.Errorf("login body contains %q; tokens belong in HttpOnly cookies only", leak)
				}
			}
		})
	}
}

// The cookie is a real credential to the gateway: an authenticated route works
// with the session cookie alone, no Authorization header.
func TestSessionCookieAuthenticatesAPICalls(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, email := e.createLoginUser(t, "c0rrect-horse")
			_, cookies := e.loginAs(t, email, "c0rrect-horse")

			req, err := http.NewRequest("GET", e.server.URL+"/api/v1/capabilities", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			for _, c := range cookies {
				req.AddCookie(c)
			}
			resp, err := e.server.Client().Do(req)
			if err != nil {
				t.Fatalf("cookie-authenticated request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("cookie-authenticated GET = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// Wrong password and unknown user must be indistinguishable to the caller —
// differing status or body would be a user-enumeration oracle.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, email := e.createLoginUser(t, "c0rrect-horse")

			wrongPass, wrongBody, _ := e.call("POST", "/api/v1/sessions", "", "", map[string]any{
				"tenant": string(e.tenant), "email": email, "password": "not-it",
			})
			noUser, noUserBody, _ := e.call("POST", "/api/v1/sessions", "", "", map[string]any{
				"tenant": string(e.tenant), "email": "ghost@example.com", "password": "not-it",
			})

			if wrongPass != http.StatusUnauthorized || noUser != http.StatusUnauthorized {
				t.Fatalf("statuses = %d/%d, want 401/401", wrongPass, noUser)
			}
			if string(wrongBody) != string(noUserBody) {
				t.Errorf("wrong-password and unknown-user bodies differ (enumeration oracle):\n %s\n %s", wrongBody, noUserBody)
			}
		})
	}
}

// A user in tenant A must not be able to log in by naming tenant B, even with
// the correct password (INV-T1: the tenant is part of the identity).
func TestLoginIsTenantScoped(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, email := e.createLoginUser(t, "c0rrect-horse")

			other := tenancy.ID(idgen.New())
			if err := tenancy.CreateTenant(context.Background(), db, other, "other tenant"); err != nil {
				t.Fatalf("create other tenant: %v", err)
			}

			status, body, _ := e.call("POST", "/api/v1/sessions", "", "", map[string]any{
				"tenant": string(other), "email": email, "password": "c0rrect-horse",
			})
			if status != http.StatusUnauthorized {
				t.Errorf("cross-tenant login = %d, want 401; body=%s", status, body)
			}
		})
	}
}

// Logout revokes the session it was called with; the token stops working
// immediately and the cookies are cleared.
func TestLogoutRevokesTheCallersSession(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, email := e.createLoginUser(t, "c0rrect-horse")
			_, cookies := e.loginAs(t, email, "c0rrect-horse")

			sessionTok := ""
			for _, c := range cookies {
				if c.Name == cookieSession {
					sessionTok = c.Value
				}
			}
			if sessionTok == "" {
				t.Fatal("no session cookie from login")
			}

			if status, body, _ := e.call("GET", "/api/v1/capabilities", sessionTok, "", nil); status != http.StatusOK {
				t.Fatalf("pre-logout call = %d, want 200; body=%s", status, body)
			}
			if status, body, _ := e.call("DELETE", "/api/v1/sessions/current", sessionTok, "", nil); status != http.StatusNoContent {
				t.Fatalf("logout = %d, want 204; body=%s", status, body)
			}
			if status, _, _ := e.call("GET", "/api/v1/capabilities", sessionTok, "", nil); status != http.StatusUnauthorized {
				t.Errorf("post-logout call = %d, want 401 — the revoked token still works", status)
			}
		})
	}
}

// INV-T4: authentication events are attributable. Login and logout each leave an
// audit_log row carrying the actor and the session it acted on.
func TestSessionEventsAreAudited(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			userID, email := e.createLoginUser(t, "c0rrect-horse")
			_, cookies := e.loginAs(t, email, "c0rrect-horse")

			if got := auditCount(t, db, e.tenant, string(userID), identity.AuditLogin); got != 1 {
				t.Errorf("login audit rows = %d, want 1", got)
			}

			sessionTok := ""
			for _, c := range cookies {
				if c.Name == cookieSession {
					sessionTok = c.Value
				}
			}
			if status, body, _ := e.call("DELETE", "/api/v1/sessions/current", sessionTok, "", nil); status != http.StatusNoContent {
				t.Fatalf("logout = %d, want 204; body=%s", status, body)
			}
			if got := auditCount(t, db, e.tenant, string(userID), identity.AuditLogout); got != 1 {
				t.Errorf("logout audit rows = %d, want 1", got)
			}
		})
	}
}

// auditCount reads audit_log under tenant context — the table is RLS-protected
// on Postgres, so a bare query as the app role would report zero rows and the
// test would pass for the wrong reason.
func auditCount(t *testing.T, db *storage.DB, tenant tenancy.ID, actorID, action string) int {
	t.Helper()
	var n int
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(`
			SELECT COUNT(*) FROM audit_log
			WHERE tenant_id = ? AND object = 'Session' AND actor_id = ? AND action = ?`),
			string(tenant), actorID, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// jsonBody marshals v into a request body reader.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(b)
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

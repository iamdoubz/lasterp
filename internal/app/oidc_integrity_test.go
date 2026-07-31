//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-1.9 opens two more holes in the authentication fence — the OIDC start and
// callback routes — and adds a second way to mint a session. These tests hold
// the same line the password path is held to:
//
//	INV-T2  the two new routes are the *only* new public ones, and neither
//	        issues a session without a state that matches the flow it started;
//	INV-T4  a login through an IdP is as attributable as one through a password;
//	        the audit trail says which path was used.
//
// The IdP here is a fake that will mint anything asked of it. A real Keycloak
// covers the happy path in oidc_keycloak_integrity_test.go; this file covers
// what a real IdP would never do.

const (
	testOIDCClientID = "lasterp-test"
	testOIDCRedirect = "http://127.0.0.1/api/v1/sessions/oidc/callback"
)

// fakeIdP is a minimal OpenID provider: discovery, JWKS, token endpoint.
type fakeIdP struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey

	// nonce is what the next minted ID token echoes; the test sets it from the
	// flow cookie the server issued.
	nonce string
	// subject, email and emailVerified shape the assertion.
	subject       string
	email         string
	emailVerified bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &fakeIdP{t: t, key: key, subject: "idp-subject-1", emailVerified: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]string{
			"issuer":                 idp.server.URL,
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
			"jwks_uri":               idp.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{json.RawMessage(fmt.Sprintf(
			`{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":%q,"e":"AQAB"}`,
			base64.RawURLEncoding.EncodeToString(idp.key.N.Bytes())))}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"id_token": idp.mint(), "token_type": "Bearer"})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (i *fakeIdP) mint() string {
	i.t.Helper()
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{
		"iss": i.server.URL, "sub": i.subject, "aud": testOIDCClientID,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"nonce": i.nonce, "email": i.email, "email_verified": i.emailVerified,
	})
	if err != nil {
		i.t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"k1"}`))
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		i.t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// configureOIDC points the process at idp for tenant. It must run before the
// handler is built, since discovery happens at boot.
func configureOIDC(t *testing.T, idp *fakeIdP, tenant tenancy.ID) {
	t.Helper()
	t.Setenv(envOIDCIssuer, idp.server.URL)
	t.Setenv(envOIDCClientID, testOIDCClientID)
	t.Setenv(envOIDCClientSecret, "test-secret")
	t.Setenv(envOIDCRedirectURL, testOIDCRedirect)
	t.Setenv(envOIDCTenant, string(tenant))
}

// seedWithOIDC boots the product with a fake IdP wired in, and returns an env
// whose tenant is the one that IdP signs into.
func seedWithOIDC(t *testing.T, db *storage.DB) (*env, *fakeIdP) {
	t.Helper()
	idp := newFakeIdP(t)
	// seed() mints its own tenant id, so the IdP must be pointed at the same
	// one. Both derive from a single generated id.
	tenant := tenancy.ID(idgen.New())
	configureOIDC(t, idp, tenant)
	e := seedTenant(t, db, tenant)
	return e, idp
}

// oidcFlow is one login attempt in progress: what the server put in the flow
// cookie, plus the provider URL it wants the browser sent to.
type oidcFlow struct {
	state            string
	nonce            string
	authorizationURL string
	cookie           *http.Cookie
}

// startOIDC calls the start route and returns the pending flow.
func (e *env) startOIDC(t *testing.T) *oidcFlow {
	t.Helper()
	resp, err := e.server.Client().Get(e.server.URL + oidcFlowPath)
	if err != nil {
		t.Fatalf("GET %s: %v", oidcFlowPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	u, err := url.Parse(body.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}

	var flowCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieOIDC {
			flowCookie = c
		}
	}
	if flowCookie == nil {
		t.Fatal("start route set no flow cookie")
	}
	// The nonce the IdP must echo lives in the cookie the server just set —
	// exactly what a real IdP receives in the authorization request.
	_, nonce, _, ok := splitOIDCFlow(flowCookie.Value)
	if !ok {
		t.Fatalf("flow cookie is malformed: %q", flowCookie.Value)
	}
	return &oidcFlow{
		state:            u.Query().Get("state"),
		nonce:            nonce,
		authorizationURL: body.AuthorizationURL,
		cookie:           flowCookie,
	}
}

// startOIDCWith starts a flow and points the fake IdP at the nonce it must
// echo, which a real provider gets from the authorization request itself.
func (e *env) startOIDCWith(t *testing.T, idp *fakeIdP) *oidcFlow {
	t.Helper()
	flow := e.startOIDC(t)
	idp.nonce = flow.nonce
	return flow
}

// callback replays the IdP's redirect back to LastERP.
func (e *env) callback(t *testing.T, flow *oidcFlow, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", e.server.URL+oidcFlowPath+"/callback?"+query, nil)
	if err != nil {
		t.Fatalf("new callback request: %v", err)
	}
	if flow != nil && flow.cookie != nil {
		req.AddCookie(flow.cookie)
	}
	// The callback answers with a redirect on success; we want to inspect it,
	// not follow it.
	client := *e.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *oidcFlow) query(code string) string {
	return url.Values{"code": {code}, "state": {f.state}}.Encode()
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// --- the tests ---

// AC: "session/device semantics identical to password path". An OIDC login must
// produce the same three cookies with the same protections as a password login,
// and the session it mints must work on a real authenticated route.
func TestOIDCLoginIssuesTheSamePasswordPathSession(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			user := e.createOIDCUser(t, idp)

			flow := e.startOIDCWith(t, idp)
			resp := e.callback(t, flow, flow.query("auth-code"))
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("callback status = %d, want 302", resp.StatusCode)
			}

			cookies := resp.Cookies()
			for _, name := range []string{cookieSession, cookieRefresh, cookieDevice} {
				c := cookieNamed(cookies, name)
				if c == nil || c.Value == "" {
					t.Fatalf("OIDC login set no %s cookie", name)
				}
				if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
					t.Errorf("cookie %s: HttpOnly=%v Secure=%v SameSite=%v, want true/true/Strict — the OIDC path must not be a weaker way in",
						name, c.HttpOnly, c.Secure, c.SameSite)
				}
			}
			if cookieNamed(cookies, cookieRefresh).Path != refreshPath {
				t.Errorf("refresh cookie path = %q, want %q", cookieNamed(cookies, cookieRefresh).Path, refreshPath)
			}
			// The flow cookie is spent: single use, whatever the outcome.
			if spent := cookieNamed(cookies, cookieOIDC); spent == nil || spent.MaxAge >= 0 {
				t.Error("the flow cookie was not cleared at the callback")
			}

			// The session works, and belongs to the user the subject was bound
			// to — not merely to somebody.
			session := cookieNamed(cookies, cookieSession).Value
			st, body, _ := e.call("GET", "/api/v1/meta/objects", session, "", nil)
			if st != http.StatusOK {
				t.Fatalf("authenticated call with the OIDC session = %d, want 200: %s", st, body)
			}
			s, err := identity.ValidateSession(context.Background(), db, session)
			if err != nil {
				t.Fatalf("ValidateSession: %v", err)
			}
			if s.UserID != user {
				t.Errorf("session belongs to %s, want %s", s.UserID, user)
			}
			if s.TenantID != e.tenant {
				t.Errorf("session tenant = %s, want %s", s.TenantID, e.tenant)
			}
			if s.DeviceID == "" {
				t.Error("OIDC session has no device binding (INV-T4)")
			}
		})
	}
}

// The OIDC-issued session must support the rest of the session lifecycle:
// refresh is bound to the same device, and logout revokes it.
func TestOIDCSessionRefreshesAndLogsOut(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			e.createOIDCUser(t, idp)

			flow := e.startOIDCWith(t, idp)
			cookies := e.callback(t, flow, flow.query("auth-code")).Cookies()

			req, err := http.NewRequest("POST", e.server.URL+refreshPath, nil)
			if err != nil {
				t.Fatalf("new refresh request: %v", err)
			}
			req.AddCookie(cookieNamed(cookies, cookieRefresh))
			req.AddCookie(cookieNamed(cookies, cookieDevice))
			resp, err := e.server.Client().Do(req)
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("refresh of an OIDC session = %d, want 200", resp.StatusCode)
			}
			// Refreshing rotates the access token, so the cookie set here is the
			// live credential from now on — the pre-refresh one is spent.
			refreshed := cookieNamed(resp.Cookies(), cookieSession)
			if refreshed == nil || refreshed.Value == "" {
				t.Fatal("refresh set no new session cookie")
			}

			// Refreshing from a different device must be refused exactly as it
			// is on the password path.
			req2, err := http.NewRequest("POST", e.server.URL+refreshPath, nil)
			if err != nil {
				t.Fatalf("new refresh request: %v", err)
			}
			req2.AddCookie(cookieNamed(cookies, cookieRefresh))
			req2.AddCookie(&http.Cookie{Name: cookieDevice, Value: "some-other-browser"})
			resp2, err := e.server.Client().Do(req2)
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			defer func() { _ = resp2.Body.Close() }()
			if resp2.StatusCode != http.StatusUnauthorized {
				t.Errorf("refresh from another device = %d, want 401", resp2.StatusCode)
			}

			session := refreshed.Value
			if st, body, _ := e.call("DELETE", "/api/v1/sessions/current", session, "", nil); st != http.StatusNoContent {
				t.Fatalf("logout = %d, want 204: %s", st, body)
			}
			if st, _, _ := e.call("GET", "/api/v1/meta/objects", session, "", nil); st != http.StatusUnauthorized {
				t.Errorf("revoked OIDC session still works: %d", st)
			}
		})
	}
}

// INV-T4: every mutation is attributable. Issuing a session is a mutation, and
// the trail must say it came from the IdP rather than from a password.
func TestOIDCLoginIsAudited(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			user := e.createOIDCUser(t, idp)

			flow := e.startOIDCWith(t, idp)
			e.callback(t, flow, flow.query("auth-code"))

			var n int
			err := tenancy.WithTenant(context.Background(), db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, db.Rebind(`
					SELECT COUNT(*) FROM audit_log
					WHERE tenant_id = ? AND object = 'Session' AND action = ? AND actor_id = ?`),
					string(e.tenant), identity.AuditLoginOIDC, string(user)).Scan(&n)
			})
			if err != nil {
				t.Fatalf("query audit_log: %v", err)
			}
			if n != 1 {
				t.Errorf("audit rows for %s = %d, want 1 (INV-T4)", identity.AuditLoginOIDC, n)
			}
		})
	}
}

// The callback is a public route that mints a session, so it is the highest
// value target WP-1.9 adds. Each of these must come back 401 with no session.
func TestOIDCCallbackRefusals(t *testing.T) {
	tests := []struct {
		name string
		// mutate returns the query string and the flow to present (nil ⇒ no
		// flow cookie at all).
		mutate func(t *testing.T, e *env, idp *fakeIdP, flow *oidcFlow) (string, *oidcFlow)
	}{
		{
			// Without this check, anyone could complete a login the victim's
			// browser never started — classic login CSRF.
			name: "state does not match the flow cookie",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return url.Values{"code": {"auth-code"}, "state": {"attacker-chosen"}}.Encode(), flow
			},
		},
		{
			name: "no state at all",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return url.Values{"code": {"auth-code"}}.Encode(), flow
			},
		},
		{
			name: "no flow cookie",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return flow.query("auth-code"), nil
			},
		},
		{
			name: "malformed flow cookie",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return flow.query("auth-code"), &oidcFlow{
					state:  flow.state,
					cookie: &http.Cookie{Name: cookieOIDC, Value: "not-three-parts"},
				}
			},
		},
		{
			name: "no code",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return url.Values{"state": {flow.state}}.Encode(), flow
			},
		},
		{
			// The IdP reports a declined or failed login in the redirect.
			name: "provider reported an error",
			mutate: func(_ *testing.T, _ *env, _ *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				return url.Values{"error": {"access_denied"}, "state": {flow.state}}.Encode(), flow
			},
		},
		{
			// No JIT provisioning: a valid assertion for somebody with no local
			// user is still not a login (WP-1.9-decisions.md §3).
			name: "valid assertion, no local user",
			mutate: func(_ *testing.T, _ *env, idp *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				idp.email = "stranger@example.com"
				return flow.query("auth-code"), flow
			},
		},
		{
			// The account-takeover guard: an unverified email must not claim a
			// local account.
			name: "email is not verified by the provider",
			mutate: func(t *testing.T, e *env, idp *fakeIdP, flow *oidcFlow) (string, *oidcFlow) {
				e.createOIDCUser(t, idp)
				idp.emailVerified = false
				return flow.query("auth-code"), flow
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for name, db := range bootDBs(t) {
				t.Run(name, func(t *testing.T) {
					e, idp := seedWithOIDC(t, db)
					idp.email = idgen.New() + "@example.com"
					flow := e.startOIDCWith(t, idp)
					query, present := tc.mutate(t, e, idp, flow)

					resp := e.callback(t, present, query)
					if resp.StatusCode != http.StatusUnauthorized {
						t.Fatalf("callback status = %d, want 401", resp.StatusCode)
					}
					if c := cookieNamed(resp.Cookies(), cookieSession); c != nil && c.Value != "" {
						t.Error("a refused callback still issued a session cookie")
					}
				})
			}
		})
	}
}

// The flow cookie is single-use: replaying a callback that already succeeded
// must not mint a second session from the same nonce and verifier.
func TestOIDCFlowIsSingleUse(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			e.createOIDCUser(t, idp)

			flow := e.startOIDCWith(t, idp)
			if st := e.callback(t, flow, flow.query("auth-code")).StatusCode; st != http.StatusFound {
				t.Fatalf("first callback = %d, want 302", st)
			}
			// The browser no longer holds the cookie — the server cleared it —
			// so a replay arrives without flow state and is refused. Presenting
			// the captured cookie again is the stronger case: it is what an
			// attacker who scraped it would do.
			resp := e.callback(t, flow, flow.query("auth-code"))
			if resp.StatusCode == http.StatusFound {
				t.Error("replaying a spent callback minted a second session")
			}
		})
	}
}

// The flow cookie must be SameSite=Lax and nothing weaker or stronger: Strict
// would be withheld on the IdP's cross-site redirect and break every login,
// None would attach it to any cross-site request at all.
func TestOIDCFlowCookieIsLaxAndScoped(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			flow := e.startOIDCWith(t, idp)
			c := flow.cookie

			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("flow cookie SameSite = %v, want Lax (Strict is withheld on the IdP redirect — WP-1.9-decisions.md §5)", c.SameSite)
			}
			if !c.HttpOnly || !c.Secure {
				t.Errorf("flow cookie HttpOnly=%v Secure=%v, want both", c.HttpOnly, c.Secure)
			}
			if c.Path != oidcFlowPath {
				t.Errorf("flow cookie path = %q, want %q — it must not ride along on ordinary API calls", c.Path, oidcFlowPath)
			}
			if c.MaxAge <= 0 || time.Duration(c.MaxAge)*time.Second > oidcFlowTTL {
				t.Errorf("flow cookie MaxAge = %d, want a positive value no larger than %s", c.MaxAge, oidcFlowTTL)
			}
		})
	}
}

// Two concurrent login attempts must not be able to complete with each other's
// state: the second start overwrites the browser's cookie, which is the
// documented behaviour, and the first flow's state no longer matches.
func TestOIDCStateIsPerAttempt(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, idp := seedWithOIDC(t, db)
			e.createOIDCUser(t, idp)

			first := e.startOIDCWith(t, idp)
			second := e.startOIDCWith(t, idp)
			if first.state == second.state || first.cookie.Value == second.cookie.Value {
				t.Fatal("two login attempts shared state")
			}
			// The first attempt's state against the second attempt's cookie.
			resp := e.callback(t, &oidcFlow{state: first.state, cookie: second.cookie}, first.query("auth-code"))
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("crossed flows = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestNoOIDCRoutesWithoutAConfiguredProvider: a deployment with no IdP must not
// carry the routes at all — the web client branches on the 404, and a dead
// public route is surface for nothing.
func TestNoOIDCRoutesWithoutAConfiguredProvider(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envOIDCIssuer, "")
			e := seed(t, db)
			for _, path := range []string{oidcFlowPath, oidcFlowPath + "/callback"} {
				resp, err := e.server.Client().Get(e.server.URL + path)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("GET %s with no IdP configured = %d, want 404", path, resp.StatusCode)
				}
			}
		})
	}
}

// A half-configured deployment must fail to boot rather than quietly serving
// password-only: an operator who set an issuer meant to enable SSO.
func TestPartialOIDCConfigurationFailsBoot(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			for _, missing := range []string{envOIDCClientID, envOIDCClientSecret, envOIDCRedirectURL, envOIDCTenant} {
				t.Run("missing "+missing, func(t *testing.T) {
					idp := newFakeIdP(t)
					configureOIDC(t, idp, "some-tenant")
					t.Setenv(missing, "")
					if _, err := Handler(context.Background(), db); err == nil {
						t.Errorf("Handler booted with %s unset", missing)
					}
				})
			}
		})
	}
}

// An unreachable issuer is a boot failure too — discovery at boot is the whole
// point of doing it there.
func TestUnreachableIssuerFailsBoot(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envOIDCIssuer, "http://127.0.0.1:1")
			t.Setenv(envOIDCClientID, testOIDCClientID)
			t.Setenv(envOIDCClientSecret, "s")
			t.Setenv(envOIDCRedirectURL, testOIDCRedirect)
			t.Setenv(envOIDCTenant, "some-tenant")
			if _, err := Handler(context.Background(), db); err == nil {
				t.Error("Handler booted against an unreachable issuer")
			}
		})
	}
}

// createOIDCUser creates the local user the IdP's assertion will link to, with
// the grants an ordinary signed-in user needs.
func (e *env) createOIDCUser(t *testing.T, idp *fakeIdP) identity.UserID {
	t.Helper()
	if idp.email == "" {
		idp.email = idgen.New() + "@example.com"
	}
	hash, err := identity.HashPassword(idgen.New())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(context.Background(), e.db, e.tenant, idp.email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	grantRole(t, e.db, e.tenant, user.ID, fullGrants())
	return user.ID
}

//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-1.9 AC: "OIDC login e2e against a test IdP (Keycloak container)".
//
// The fake provider in oidc_integrity_test.go proves LastERP refuses what it
// should. This proves it accepts what it should — against a real IdP, with real
// discovery, a real JWKS, real RS256 signatures, real PKCE enforcement and a
// real login form. Nothing here is stubbed: the only thing standing in for a
// browser is the code that posts the login form and follows the redirect.

const (
	keycloakImage  = "quay.io/keycloak/keycloak:26.4"
	keycloakRealm  = "lasterp"
	keycloakClient = "lasterp"
	keycloakSecret = "test-client-secret"
	keycloakUser   = "alice"
	keycloakPass   = "s3cret!"
	keycloakEmail  = "alice@example.com"

	// The redirect URI is registered with the client but never fetched by
	// Keycloak — it hands the browser a 302 and the browser goes there. The test
	// is the browser, so a fixed placeholder is enough and avoids having to know
	// the httptest server's port before the handler is built.
	keycloakRedirect = "http://127.0.0.1:9999/api/v1/sessions/oidc/callback"
)

// keycloakRealmJSON is imported at container start, so the realm, the
// confidential client and the user all exist before the first request. It is
// the whole IdP configuration this WP depends on, in one reviewable blob.
const keycloakRealmJSON = `{
  "realm": "` + keycloakRealm + `",
  "enabled": true,
  "sslRequired": "none",
  "clients": [{
    "clientId": "` + keycloakClient + `",
    "enabled": true,
    "protocol": "openid-connect",
    "publicClient": false,
    "secret": "` + keycloakSecret + `",
    "standardFlowEnabled": true,
    "directAccessGrantsEnabled": false,
    "redirectUris": ["*"],
    "webOrigins": ["*"],
    "attributes": { "pkce.code.challenge.method": "S256" }
  }],
  "users": [{
    "username": "` + keycloakUser + `",
    "email": "` + keycloakEmail + `",
    "emailVerified": true,
    "enabled": true,
    "firstName": "Alice",
    "lastName": "Example",
    "credentials": [{ "type": "password", "value": "` + keycloakPass + `", "temporary": false }]
  }]
}`

// startKeycloak boots a Keycloak with the realm imported and returns its issuer
// URL. Started once per test run: Keycloak takes tens of seconds and nothing
// here mutates it.
func startKeycloak(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        keycloakImage,
			ExposedPorts: []string{"8080/tcp"},
			Cmd:          []string{"start-dev", "--import-realm"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
			},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(keycloakRealmJSON),
				ContainerFilePath: "/opt/keycloak/data/import/realm.json",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForHTTP("/realms/" + keycloakRealm + "/.well-known/openid-configuration").
				WithPort("8080/tcp").
				WithStartupTimeout(4 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keycloak: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	// Keycloak derives the issuer it advertises from the request's Host header
	// in dev mode, so the URL we fetch discovery through must be the URL we
	// configure — oidc.Discover requires them to match exactly.
	return fmt.Sprintf("http://%s:%s/realms/%s", host, port.Port(), keycloakRealm)
}

// TestOIDCLoginAgainstKeycloak is the acceptance test: a user with a local
// LastERP account signs in through a real IdP and ends up with a working
// LastERP session.
func TestOIDCLoginAgainstKeycloak(t *testing.T) {
	issuer := startKeycloak(t)

	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			tenant := tenancy.ID(idgen.New())
			t.Setenv(envOIDCIssuer, issuer)
			t.Setenv(envOIDCClientID, keycloakClient)
			t.Setenv(envOIDCClientSecret, keycloakSecret)
			t.Setenv(envOIDCRedirectURL, keycloakRedirect)
			t.Setenv(envOIDCTenant, string(tenant))

			e := seedTenant(t, db, tenant)

			// The local user the IdP's assertion links to. Created by an
			// administrator, as it must be — LastERP never provisions from an
			// assertion.
			hash, err := identity.HashPassword(idgen.New())
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
			user, err := identity.CreateUser(context.Background(), db, tenant, keycloakEmail, hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			grantRole(t, db, tenant, user.ID, fullGrants())

			// 1. LastERP starts the flow and hands back the provider's URL.
			flow := e.startOIDC(t)

			// 2. The "browser" authenticates at Keycloak and is redirected back
			//    with an authorization code.
			code, state := loginAtKeycloak(t, flow.authorizationURL)
			if state != flow.state {
				t.Fatalf("Keycloak returned state %q, want %q", state, flow.state)
			}

			// 3. LastERP completes the flow.
			resp := e.callback(t, flow, url.Values{"code": {code}, "state": {state}}.Encode())
			if resp.StatusCode != http.StatusFound {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("callback status = %d, want 302: %s", resp.StatusCode, body)
			}

			session := cookieNamed(resp.Cookies(), cookieSession)
			if session == nil || session.Value == "" {
				t.Fatal("no session cookie after a successful Keycloak login")
			}

			// The session is real: it authenticates a real API call, and it
			// belongs to the user the IdP's subject was linked to.
			if st, body, _ := e.call("GET", "/api/v1/meta/objects", session.Value, "", nil); st != http.StatusOK {
				t.Fatalf("authenticated call after Keycloak login = %d, want 200: %s", st, body)
			}
			s, err := identity.ValidateSession(context.Background(), db, session.Value)
			if err != nil {
				t.Fatalf("ValidateSession: %v", err)
			}
			if s.UserID != user.ID {
				t.Errorf("session belongs to %s, want %s", s.UserID, user.ID)
			}

			// The subject was bound on first use, so a second login resolves
			// without touching the email claim at all.
			linked, err := identity.GetUserByOIDCSubject(context.Background(), db, tenant, issuer, subjectOf(t, db, tenant, user.ID))
			if err != nil {
				t.Fatalf("the first Keycloak login did not bind the subject: %v", err)
			}
			if linked.ID != user.ID {
				t.Errorf("subject bound to %s, want %s", linked.ID, user.ID)
			}
		})
	}
}

// loginAtKeycloak drives the hosted login form: fetch the authorization URL,
// post the credentials to the form's action, and read the code and state out of
// the redirect Keycloak answers with. This is precisely what a browser does,
// minus the rendering.
func loginAtKeycloak(t *testing.T, authorizationURL string) (code, state string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		// Stop at the redirect to our redirect_uri: nothing is listening there,
		// and its query string is the whole point.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Get(authorizationURL)
	if err != nil {
		t.Fatalf("GET authorization URL: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read login page: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login page status = %d: %s", resp.StatusCode, body)
	}

	action := loginFormAction(t, string(body))
	form := url.Values{"username": {keycloakUser}, "password": {keycloakPass}}
	req, err := http.NewRequest("POST", action, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new form post: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	authed, err := client.Do(req)
	if err != nil {
		t.Fatalf("post login form: %v", err)
	}
	defer func() { _ = authed.Body.Close() }()
	if authed.StatusCode != http.StatusFound && authed.StatusCode != http.StatusSeeOther {
		page, _ := io.ReadAll(authed.Body)
		t.Fatalf("login form post status = %d, want a redirect; page: %s", authed.StatusCode, truncate(string(page), 800))
	}

	location, err := url.Parse(authed.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	q := location.Query()
	if q.Get("code") == "" {
		t.Fatalf("Keycloak redirected without a code: %s", location)
	}
	return q.Get("code"), q.Get("state")
}

// loginFormAction pulls the form target out of Keycloak's login page. The page
// is server-rendered HTML with exactly one login form.
var loginFormRE = regexp.MustCompile(`(?i)<form[^>]*\bid="kc-form-login"[^>]*\baction="([^"]+)"`)

func loginFormAction(t *testing.T, page string) string {
	t.Helper()
	m := loginFormRE.FindStringSubmatch(page)
	if m == nil {
		// Fall back to any form with an action, so a Keycloak theme change
		// renames the form rather than breaking the whole test.
		alt := regexp.MustCompile(`(?i)<form[^>]*\baction="([^"]+)"`).FindStringSubmatch(page)
		if alt == nil {
			t.Fatalf("no login form in Keycloak's response: %s", truncate(page, 800))
		}
		m = alt
	}
	// Attributes are HTML-escaped: the action carries &amp; between parameters.
	return html.UnescapeString(m[1])
}

// subjectOf reads the OIDC subject the login bound to the user, so the test can
// assert the binding without knowing Keycloak's generated user id.
func subjectOf(t *testing.T, db *storage.DB, tenant tenancy.ID, id identity.UserID) string {
	t.Helper()
	var subject sql.NullString
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT oidc_subject FROM users WHERE tenant_id = ? AND id = ?`),
			string(tenant), string(id)).Scan(&subject)
	})
	if err != nil {
		t.Fatalf("read bound subject: %v", err)
	}
	if !subject.Valid || subject.String == "" {
		t.Fatal("the Keycloak login bound no subject to the user")
	}
	return subject.String
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

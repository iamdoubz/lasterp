// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/oidc"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// cookieOIDC holds one pending login: the CSRF state, the nonce the ID token
// must echo, and the PKCE code verifier. It is not a credential — everything in
// it is single-use and worthless without the authorization code the IdP hands
// back — which is what makes SameSite=Lax acceptable here (see below).
const cookieOIDC = "lasterp_oidc"

// oidcFlowPath scopes the flow cookie to the two OIDC routes, so it is not
// attached to ordinary API calls.
const oidcFlowPath = "/api/v1/sessions/oidc"

// oidcFlowTTL bounds how long a started login stays completable. Ten minutes is
// generous for typing a password at an IdP and short enough that an abandoned
// flow is not a lingering artifact.
const oidcFlowTTL = 10 * time.Minute

// Environment variables configuring the relying party. Provider configuration
// is deployment-level rather than per-tenant because a per-tenant table would
// have to store an OAuth client secret at rest and no secrets vault exists yet
// (WP-1.9-decisions.md §2).
const (
	envOIDCIssuer       = "LASTERP_OIDC_ISSUER"
	envOIDCClientID     = "LASTERP_OIDC_CLIENT_ID"
	envOIDCClientSecret = "LASTERP_OIDC_CLIENT_SECRET"
	envOIDCRedirectURL  = "LASTERP_OIDC_REDIRECT_URL"
	envOIDCTenant       = "LASTERP_OIDC_TENANT"
)

// oidcLogin is the wired OIDC login flow. A nil *oidcLogin means the deployment
// has no IdP configured, and registers no routes at all.
type oidcLogin struct {
	db       *storage.DB
	provider *identity.OIDCProvider
	tenant   tenancy.ID
	spent    spentStates
}

// spentStates remembers the states of completed logins so one flow cannot be
// completed twice. The flow cookie alone cannot enforce this: the server clears
// it, but an attacker holding a copy is not bound by that.
//
// This is defense in depth, not the primary control — OAuth 2.0 §4.1.2 already
// requires the provider to reject a reused authorization code, so a replay
// fails at the token endpoint of any conformant IdP. Which is why it can afford
// to be process-local:
//
// ponytail: per-process map, so a replay landing on a *different* replica is
// caught by the IdP rather than here. Move it to a shared store (or a
// consumed_flows table) if replay protection ever has to hold without a
// conformant provider behind it.
type spentStates struct {
	mu sync.Mutex
	at map[string]time.Time
}

// consume records state and reports whether this is its first use.
func (s *spentStates) consume(state string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = make(map[string]time.Time)
	}
	// Sweep on insert. A state older than the flow TTL cannot complete a login
	// anyway, so the map is bounded by the number of logins in one TTL window —
	// itself bounded by the rate limiter on this public route.
	for old, t := range s.at {
		if now.Sub(t) > oidcFlowTTL {
			delete(s.at, old)
		}
	}
	if _, seen := s.at[state]; seen {
		return false
	}
	s.at[state] = now
	return true
}

// newOIDCLogin discovers the configured provider, or returns (nil, nil) when
// none is configured. A partially configured deployment is an error rather than
// a silent fallback to password-only: an operator who set an issuer and fumbled
// the secret meant to enable SSO, and should be told so at boot instead of
// discovering it when a user cannot sign in.
func newOIDCLogin(ctx context.Context, db *storage.DB) (*oidcLogin, error) {
	issuer := os.Getenv(envOIDCIssuer)
	if issuer == "" {
		return nil, nil
	}
	cfg := oidc.Config{
		Issuer:       issuer,
		ClientID:     os.Getenv(envOIDCClientID),
		ClientSecret: os.Getenv(envOIDCClientSecret),
		RedirectURL:  os.Getenv(envOIDCRedirectURL),
	}
	tenant := tenancy.ID(os.Getenv(envOIDCTenant))
	if tenant == "" {
		return nil, fmt.Errorf("app: %s is set but %s is not: there is nothing to say which tenant the IdP signs users into", envOIDCIssuer, envOIDCTenant)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("app: %s is set but %s, %s or %s is missing", envOIDCIssuer, envOIDCClientID, envOIDCClientSecret, envOIDCRedirectURL)
	}
	client, err := oidc.Discover(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("app: discover OIDC provider: %w", err)
	}
	return &oidcLogin{db: db, provider: &identity.OIDCProvider{DB: db, Client: client}, tenant: tenant}, nil
}

// oidcActions returns the login routes, or nothing when no IdP is configured.
// Both are Public — two more authentication bypasses, which the route fence in
// routes_integrity_test.go pins down (INV-T2) — and both are reads in the
// gateway's sense: Action.Write means "requires an Idempotency-Key", and no IdP
// will send one on its redirect.
func oidcActions(o *oidcLogin) []api.Action {
	if o == nil {
		return nil
	}
	return []api.Action{
		{Method: "GET", Path: oidcFlowPath, Public: true,
			Summary: "Begin an OIDC login and get the provider's authorization URL", Handler: o.start},
		{Method: "GET", Path: oidcFlowPath + "/callback", Public: true,
			Summary: "Complete an OIDC login started at the provider", Handler: o.callback},
	}
}

// start mints a login attempt: state, nonce and PKCE verifier into a
// short-lived cookie, the authorization URL back to the caller. The web client
// also uses this route's presence to decide whether to offer SSO at all — a 404
// means no IdP is configured.
func (o *oidcLogin) start(w http.ResponseWriter, r *http.Request, _ tenancy.ID) {
	req, err := o.provider.Client.Start()
	if err != nil {
		fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieOIDC,
		Value:    strings.Join([]string{req.State, req.Nonce, req.CodeVerifier}, "."),
		Path:     oidcFlowPath,
		MaxAge:   int(oidcFlowTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		// SameSite=Lax, uniquely among this product's cookies. Strict is
		// correct for the session cookies and *wrong* here: a Strict cookie is
		// withheld on a top-level cross-site navigation, which is exactly what
		// the IdP's redirect to the callback is, so every login would arrive
		// with no flow state and fail. See WP-1.9-decisions.md §5.
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authorization_url": req.URL})
}

// callback completes the flow: verify the state, redeem the code, resolve the
// assertion to a local user, and issue exactly the session the password path
// issues.
func (o *oidcLogin) callback(w http.ResponseWriter, r *http.Request, _ tenancy.ID) {
	state, nonce, verifier, ok := splitOIDCFlow(cookieValue(r, cookieOIDC))
	// The flow cookie is single-use whatever happens next: a failed attempt
	// must not leave a replayable nonce or verifier in the browser.
	clearOIDCCookie(w)

	q := r.URL.Query()
	// Constant-time, though the state is not a long-lived secret: it costs
	// nothing and stops this being the one comparison someone later cites as
	// precedent.
	if !ok || subtle.ConstantTimeCompare([]byte(state), []byte(q.Get("state"))) != 1 {
		unauthorized(w, r)
		return
	}
	// The IdP reports user-declined and error conditions in the redirect rather
	// than by failing the request.
	if q.Get("error") != "" || q.Get("code") == "" {
		unauthorized(w, r)
		return
	}
	// One flow, one session. Checked after the state matches so an attacker
	// cannot burn someone else's pending flow by guessing at states.
	if !o.spent.consume(state, time.Now()) {
		unauthorized(w, r)
		return
	}

	user, err := o.provider.Authenticate(r.Context(), o.tenant, identity.Credentials{
		Code: q.Get("code"), CodeVerifier: verifier, Nonce: nonce,
	})
	if errors.Is(err, identity.ErrInvalidCredentials) {
		unauthorized(w, r)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}

	// From here the two login paths are the same code: same session store, same
	// device binding, same cookies, same audit trail.
	device := deviceID(r)
	issued, err := identity.IssueSession(r.Context(), o.db, o.tenant, user, device)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := identity.AuditSession(r.Context(), o.db, o.tenant, user, issued.ID, identity.AuditLoginOIDC); err != nil {
		fail(w, r, err)
		return
	}
	setSessionCookies(w, issued, device)
	// The browser arrived here by navigation, so it gets a navigation back —
	// there is no client-side code running to read a JSON body.
	http.Redirect(w, r, "/", http.StatusFound)
}

// splitOIDCFlow unpacks the flow cookie. Its three parts are base64url, so "."
// is an unambiguous separator.
func splitOIDCFlow(v string) (state, nonce, verifier string, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func clearOIDCCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieOIDC, Value: "", Path: oidcFlowPath, MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

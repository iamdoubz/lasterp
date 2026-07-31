// SPDX-License-Identifier: AGPL-3.0-only

// Package oidc is an OpenID Connect relying party: provider discovery, the
// authorization-code flow with PKCE, and ID-token validation. It is a leaf
// package with no LastERP imports and no third-party ones — ID-token signatures
// are verified directly over stdlib crypto rather than through a JOSE library
// (ADR-019, jose.go).
//
// LastERP is a confidential client: the ID token is fetched server-side from
// the token endpoint over TLS with client authentication and never travels
// through the browser. Signature verification is therefore defense in depth
// rather than the only control — which is why no front-channel (implicit or
// hybrid) flow is implemented and none should be added.
//
// Who may log in is not this package's business: it authenticates an assertion
// and hands back claims. Mapping a subject to a LastERP user, and refusing when
// there isn't one, is kernel/identity's job.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// leeway absorbs clock skew between LastERP and the IdP when checking exp/iat.
// One minute is the conventional allowance; larger would meaningfully extend
// the life of an expired token.
const leeway = time.Minute

// maxResponseBytes caps every response read from the IdP. The IdP is a trusted
// party but a remote one, and an unbounded io.ReadAll against a compromised or
// merely broken endpoint is an easy way to lose the server.
const maxResponseBytes = 1 << 20

// minJWKSRefetch throttles JWKS refetches triggered by an unknown kid, so a
// stream of tokens bearing junk kids cannot be turned into a request amplifier
// pointed at the IdP. The cost is that a key rotation is picked up after at
// most this long rather than instantly — IdPs publish the new key before
// signing with it, so in practice the window never opens.
const minJWKSRefetch = time.Minute

// Config is the relying-party configuration. It comes from deployment
// environment variables (WP-1.9-decisions.md §2); there is no per-tenant
// provider configuration until a secrets vault exists to hold a client secret
// at rest.
type Config struct {
	// Issuer is the IdP's issuer URL. The discovery document must claim
	// exactly this value.
	Issuer string
	// ClientID and ClientSecret authenticate LastERP to the token endpoint.
	ClientID     string
	ClientSecret string
	// RedirectURL is this deployment's callback route, registered with the
	// IdP.
	RedirectURL string
	// Scopes to request. Defaults to openid, profile, email.
	Scopes []string
	// HTTPClient overrides the client used for discovery, JWKS and token
	// requests. Defaults to a client with a sane timeout.
	HTTPClient *http.Client
	// Now overrides the clock for exp/iat checks in tests.
	Now func() time.Time
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return []string{"openid", "profile", "email"}
}

// Client is a discovered OIDC provider ready to run the code flow. It is safe
// for concurrent use.
type Client struct {
	cfg      Config
	authzURL string
	tokenURL string
	jwksURL  string

	mu        sync.Mutex
	keys      *keySet
	fetchedAt time.Time
}

// Discover fetches the provider's OpenID configuration and prepares a Client.
// It is called once at boot: a deployment configured with an unreachable or
// mismatched issuer should fail to start rather than fail at first login.
func Discover(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: issuer, client id, client secret and redirect URL are all required")
	}
	var doc struct {
		Issuer   string `json:"issuer"`
		AuthzURL string `json:"authorization_endpoint"`
		TokenURL string `json:"token_endpoint"`
		JWKSURL  string `json:"jwks_uri"`
	}
	wellKnown := strings.TrimSuffix(cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := getJSON(ctx, cfg.httpClient(), wellKnown, &doc); err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	// OIDC Discovery §4.3 requires this comparison. Skipping it would let a
	// provider that merely answers at the configured URL assert any issuer it
	// likes, and issuer is what we later bind a user's identity to.
	if doc.Issuer != cfg.Issuer {
		return nil, fmt.Errorf("oidc: discovery document issuer %q does not match configured issuer %q", doc.Issuer, cfg.Issuer)
	}
	if doc.AuthzURL == "" || doc.TokenURL == "" || doc.JWKSURL == "" {
		return nil, errors.New("oidc: discovery document is missing a required endpoint")
	}
	c := &Client{cfg: cfg, authzURL: doc.AuthzURL, tokenURL: doc.TokenURL, jwksURL: doc.JWKSURL}
	if _, err := c.keySet(ctx, true); err != nil {
		return nil, err
	}
	return c, nil
}

// AuthRequest is one pending login. Everything in it except URL must be carried
// across the redirect and handed back to Exchange: State proves the callback
// belongs to a flow this browser started, CodeVerifier proves the code is being
// redeemed by whoever requested it (PKCE), and Nonce binds the resulting ID
// token to this request.
type AuthRequest struct {
	URL          string
	State        string
	Nonce        string
	CodeVerifier string
}

// Start mints a new authorization request. The caller is responsible for
// keeping State, Nonce and CodeVerifier until the callback — in LastERP they go
// into one short-lived cookie (WP-1.9-decisions.md §5).
func (c *Client) Start() (*AuthRequest, error) {
	state, err := randomString()
	if err != nil {
		return nil, err
	}
	nonce, err := randomString()
	if err != nil {
		return nil, err
	}
	verifier, err := randomString()
	if err != nil {
		return nil, err
	}
	challenge := sha256.Sum256([]byte(verifier))

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", strings.Join(c.cfg.scopes(), " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")

	sep := "?"
	if strings.Contains(c.authzURL, "?") {
		sep = "&"
	}
	return &AuthRequest{
		URL:          c.authzURL + sep + q.Encode(),
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
	}, nil
}

// Claims are the ID-token claims LastERP uses. Everything else the IdP sends is
// ignored: authorization is local (roles come from kernel/authz, never from the
// token), so group and role claims are deliberately not read.
type Claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	Expiry   int64    `json:"exp"`
	IssuedAt int64    `json:"iat"`
	Nonce    string   `json:"nonce"`
	// AuthorizedParty is required when the token has more than one audience.
	AuthorizedParty string `json:"azp"`
	Email           string `json:"email"`
	EmailVerified   bool   `json:"email_verified"`
}

// audience decodes the "aud" claim, which RFC 7519 allows to be either a single
// string or an array of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("oidc: aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

func (a audience) contains(s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// Exchange redeems an authorization code for an ID token and returns its
// validated claims. nonce must be the value from the AuthRequest that started
// this flow.
//
// Errors wrapping ErrInvalidToken mean the assertion was rejected and the
// caller should answer "authentication failed"; any other error is a transport
// or provider fault and is not the user's doing.
func (c *Client) Exchange(ctx context.Context, code, codeVerifier, nonce string) (*Claims, error) {
	if code == "" || codeVerifier == "" || nonce == "" {
		return nil, fmt.Errorf("%w: incomplete authorization response", ErrInvalidToken)
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oidc: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic: the secret goes in the Authorization header rather
	// than the form body, so it stays out of any intermediary that logs
	// request bodies.
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))

	resp, err := c.cfg.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("oidc: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// A rejected code (expired, replayed, wrong verifier) is an
		// authentication failure, not a server fault — the IdP answers 400 for
		// all of them.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%w: token endpoint rejected the code (%d)", ErrInvalidToken, resp.StatusCode)
		}
		return nil, fmt.Errorf("oidc: token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("oidc: parse token response: %w", err)
	}
	if tok.IDToken == "" {
		return nil, fmt.Errorf("%w: token response carried no id_token", ErrInvalidToken)
	}
	return c.verifyIDToken(ctx, tok.IDToken, nonce)
}

func (c *Client) verifyIDToken(ctx context.Context, token, nonce string) (*Claims, error) {
	ks, err := c.keySet(ctx, false)
	if err != nil {
		return nil, err
	}
	payload, err := verify(ks, token)
	if errors.Is(err, ErrInvalidToken) && strings.Contains(err.Error(), "no signing key") {
		// The one recoverable verification failure: the IdP has rotated its
		// keys since we last fetched. Refetch once (throttled) and retry.
		refreshed, ferr := c.keySet(ctx, true)
		if ferr == nil {
			payload, err = verify(refreshed, token)
		}
	}
	if err != nil {
		return nil, err
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims are not JSON", ErrInvalidToken)
	}
	if err := c.validateClaims(&claims, nonce); err != nil {
		return nil, err
	}
	return &claims, nil
}

// validateClaims applies OIDC Core §3.1.3.7. Each check is separate so a
// failure names itself in the server log, even though the caller collapses them
// all into one opaque failure for the user.
func (c *Client) validateClaims(claims *Claims, nonce string) error {
	if claims.Issuer != c.cfg.Issuer {
		return fmt.Errorf("%w: issuer %q is not %q", ErrInvalidToken, claims.Issuer, c.cfg.Issuer)
	}
	if claims.Subject == "" {
		return fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	if !claims.Audience.contains(c.cfg.ClientID) {
		return fmt.Errorf("%w: audience does not include this client", ErrInvalidToken)
	}
	// With multiple audiences the token was minted for more than one relying
	// party, and only azp says which one asked for it. Accepting it without
	// that check would accept a token another client obtained.
	if len(claims.Audience) > 1 && claims.AuthorizedParty != c.cfg.ClientID {
		return fmt.Errorf("%w: multi-audience token is not authorized for this client", ErrInvalidToken)
	}
	if claims.Nonce == "" || claims.Nonce != nonce {
		return fmt.Errorf("%w: nonce does not match this login attempt", ErrInvalidToken)
	}
	now := c.cfg.now()
	if claims.Expiry == 0 || now.After(time.Unix(claims.Expiry, 0).Add(leeway)) {
		return fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if claims.IssuedAt == 0 || time.Unix(claims.IssuedAt, 0).After(now.Add(leeway)) {
		return fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	return nil
}

// keySet returns the cached JWKS, fetching it when absent or when force is set
// and the throttle allows.
func (c *Client) keySet(ctx context.Context, force bool) (*keySet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil && !force {
		return c.keys, nil
	}
	if c.keys != nil && force && c.cfg.now().Sub(c.fetchedAt) < minJWKSRefetch {
		return c.keys, nil
	}
	var raw json.RawMessage
	if err := getJSON(ctx, c.cfg.httpClient(), c.jwksURL, &raw); err != nil {
		return nil, fmt.Errorf("oidc: fetch JWKS: %w", err)
	}
	ks, err := parseJWKS(raw)
	if err != nil {
		return nil, err
	}
	c.keys, c.fetchedAt = ks, c.cfg.now()
	return ks, nil
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d", endpoint, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// randomString returns 256 bits of base64url randomness, the same strength
// kernel/identity uses for session tokens. It is used for state, nonce and the
// PKCE verifier — a guessable value in any of the three undoes the protection
// that value exists to provide.
func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

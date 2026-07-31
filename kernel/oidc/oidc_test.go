// SPDX-License-Identifier: AGPL-3.0-only

package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The Keycloak container proves this package works against a real IdP
// (internal/app). These tests do the opposite job: a fake IdP that will emit
// any malformed or hostile token asked of it, which a real one never would.

const testClientID = "lasterp"

// fakeIdP serves discovery, JWKS and a token endpoint, and mints ID tokens to
// order.
type fakeIdP struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string

	// issuerOverride, when set, is what the discovery document claims instead
	// of the server's own URL.
	issuerOverride string
	// omitEndpoint blanks one discovery field.
	omitEndpoint string
	// idToken is what the token endpoint returns. Empty ⇒ a well-formed token
	// for the pending nonce.
	idToken string
	// noIDToken makes the token endpoint answer 200 with no id_token field.
	noIDToken bool
	// tokenStatus overrides the token endpoint's status code.
	tokenStatus int
	// nonce is the value the next minted token carries.
	nonce string
	// claims overrides the default claim set.
	claims map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &fakeIdP{t: t, key: key, kid: "idp-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]string{
			"issuer":                 idp.issuer(),
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
			"jwks_uri":               idp.server.URL + "/jwks",
		}
		if idp.omitEndpoint != "" {
			doc[idp.omitEndpoint] = ""
		}
		writeJSON(w, doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{json.RawMessage(fmt.Sprintf(
			`{"kty":"RSA","kid":%q,"alg":"RS256","use":"sig","n":%q,"e":"AQAB"}`,
			idp.kid, base64.RawURLEncoding.EncodeToString(idp.key.N.Bytes())))}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if idp.tokenStatus != 0 {
			w.WriteHeader(idp.tokenStatus)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		// Client authentication is not optional: assert the RP actually sent
		// it, so a regression that drops the secret is caught here.
		if id, secret, ok := r.BasicAuth(); !ok || id != testClientID || secret == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if idp.noIDToken {
			writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer"})
			return
		}
		token := idp.idToken
		if token == "" {
			token = idp.mint(nil)
		}
		writeJSON(w, map[string]any{"id_token": token, "token_type": "Bearer"})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (i *fakeIdP) issuer() string {
	if i.issuerOverride != "" {
		return i.issuerOverride
	}
	return i.server.URL
}

// mint signs an ID token, applying overrides on top of a valid claim set.
func (i *fakeIdP) mint(overrides map[string]any) string {
	i.t.Helper()
	now := time.Now().UTC()
	claims := map[string]any{
		"iss":            i.server.URL,
		"sub":            "idp-subject-1",
		"aud":            testClientID,
		"exp":            now.Add(5 * time.Minute).Unix(),
		"iat":            now.Unix(),
		"nonce":          i.nonce,
		"email":          "alice@example.com",
		"email_verified": true,
	}
	for k, v := range i.claims {
		claims[k] = v
	}
	for k, v := range overrides {
		claims[k] = v
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		i.t.Fatalf("marshal claims: %v", err)
	}
	return i.signWith(i.kid, payload)
}

func (i *fakeIdP) signWith(kid string, payload []byte) string {
	i.t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"alg":"RS256","typ":"JWT","kid":%q}`, kid)))
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		i.t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (i *fakeIdP) config() Config {
	return Config{
		Issuer:       i.issuer(),
		ClientID:     testClientID,
		ClientSecret: "shhh",
		RedirectURL:  "https://erp.example.com/api/v1/sessions/oidc/callback",
	}
}

func (i *fakeIdP) discover(t *testing.T) *Client {
	t.Helper()
	c, err := Discover(context.Background(), i.config())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestDiscover(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		idp := newFakeIdP(t)
		c := idp.discover(t)
		if c.tokenURL != idp.server.URL+"/token" {
			t.Errorf("tokenURL = %q", c.tokenURL)
		}
		// Discovery must have pulled the JWKS, so a later login does not
		// discover a broken IdP at the worst moment.
		if c.keys == nil {
			t.Error("Discover did not fetch the JWKS")
		}
	})

	t.Run("issuer mismatch is fatal", func(t *testing.T) {
		idp := newFakeIdP(t)
		// The document claims an issuer other than the one we configured and
		// fetched from — OIDC Discovery §4.3. Accepting it would let whoever
		// answers at the configured URL assert any identity namespace.
		idp.issuerOverride = "https://evil.example.com"
		cfg := idp.config()
		cfg.Issuer = idp.server.URL
		if _, err := Discover(context.Background(), cfg); err == nil {
			t.Fatal("Discover accepted a mismatched issuer")
		}
	})

	t.Run("missing endpoint is fatal", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.omitEndpoint = "token_endpoint"
		if _, err := Discover(context.Background(), idp.config()); err == nil {
			t.Fatal("Discover accepted a document with no token endpoint")
		}
	})

	t.Run("incomplete config is refused", func(t *testing.T) {
		idp := newFakeIdP(t)
		cfg := idp.config()
		cfg.ClientSecret = ""
		if _, err := Discover(context.Background(), cfg); err == nil {
			t.Fatal("Discover accepted a config with no client secret")
		}
	})

	t.Run("unreachable issuer is fatal", func(t *testing.T) {
		cfg := Config{
			Issuer: "http://127.0.0.1:1", ClientID: "x", ClientSecret: "y", RedirectURL: "z",
			HTTPClient: &http.Client{Timeout: time.Second},
		}
		if _, err := Discover(context.Background(), cfg); err == nil {
			t.Fatal("Discover succeeded against an unreachable issuer")
		}
	})
}

func TestStartBuildsAnAuthorizationRequest(t *testing.T) {
	idp := newFakeIdP(t)
	c := idp.discover(t)

	req, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type":         "code",
		"client_id":             testClientID,
		"redirect_uri":          idp.config().RedirectURL,
		"state":                 req.State,
		"nonce":                 req.Nonce,
		"code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope %q does not request openid", q.Get("scope"))
	}
	// PKCE: the challenge must be S256 of the verifier, not the verifier
	// itself — sending the plain verifier would defeat the point.
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	if got, want := q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("code_challenge = %q, want S256(verifier) = %q", got, want)
	}
	if req.CodeVerifier == q.Get("code_challenge") {
		t.Error("the plain code verifier was sent as the challenge")
	}

	second, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if second.State == req.State || second.Nonce == req.Nonce || second.CodeVerifier == req.CodeVerifier {
		t.Error("two login attempts reused state, nonce or verifier")
	}
}

func TestExchangeHappyPath(t *testing.T) {
	idp := newFakeIdP(t)
	c := idp.discover(t)
	req, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	idp.nonce = req.Nonce

	claims, err := c.Exchange(context.Background(), "auth-code", req.CodeVerifier, req.Nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "idp-subject-1" {
		t.Errorf("subject = %q", claims.Subject)
	}
	if claims.Email != "alice@example.com" || !claims.EmailVerified {
		t.Errorf("email = %q verified = %v", claims.Email, claims.EmailVerified)
	}
	if claims.Issuer != idp.server.URL {
		t.Errorf("issuer = %q", claims.Issuer)
	}
}

// TestExchangeRejectsBadAssertions walks OIDC Core §3.1.3.7. Every case must
// come back wrapping ErrInvalidToken so the caller renders one opaque failure.
func TestExchangeRejectsBadAssertions(t *testing.T) {
	tests := []struct {
		name string
		// setup mutates the IdP and returns the nonce to validate against.
		setup func(t *testing.T, idp *fakeIdP, req *AuthRequest) string
	}{
		{
			name: "nonce belongs to a different login attempt",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = "some-other-attempt"
				return req.Nonce
			},
		},
		{
			name: "no nonce at all",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.idToken = idp.mint(map[string]any{"nonce": ""})
				return req.Nonce
			},
		},
		{
			name: "expired",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"exp": time.Now().Add(-2 * time.Hour).Unix()})
				return req.Nonce
			},
		},
		{
			name: "issued in the future",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"iat": time.Now().Add(2 * time.Hour).Unix()})
				return req.Nonce
			},
		},
		{
			name: "no expiry",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"exp": 0})
				return req.Nonce
			},
		},
		{
			name: "issuer is not the configured one",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"iss": "https://evil.example.com"})
				return req.Nonce
			},
		},
		{
			name: "audience is another client",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"aud": "some-other-app"})
				return req.Nonce
			},
		},
		{
			// A token minted for several relying parties says which one asked
			// for it in azp. Without a matching azp this is another client's
			// token that merely lists us.
			name: "multiple audiences without a matching azp",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{
					"aud": []string{testClientID, "some-other-app"},
					"azp": "some-other-app",
				})
				return req.Nonce
			},
		},
		{
			name: "no subject",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				idp.idToken = idp.mint(map[string]any{"sub": ""})
				return req.Nonce
			},
		},
		{
			name: "signed by a key the IdP does not publish",
			setup: func(t *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.nonce = req.Nonce
				other := newFakeIdP(t)
				other.nonce = req.Nonce
				other.claims = map[string]any{"iss": idp.server.URL}
				// Same kid, different key: the signature cannot verify.
				idp.idToken = other.signWith(idp.kid, []byte(fmt.Sprintf(
					`{"iss":%q,"sub":"x","aud":%q,"exp":%d,"iat":%d,"nonce":%q}`,
					idp.server.URL, testClientID, time.Now().Add(time.Hour).Unix(), time.Now().Unix(), req.Nonce)))
				return req.Nonce
			},
		},
		{
			name: "token response carries no id_token",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.noIDToken = true
				return req.Nonce
			},
		},
		{
			name: "token endpoint rejects the code",
			setup: func(_ *testing.T, idp *fakeIdP, req *AuthRequest) string {
				idp.tokenStatus = http.StatusBadRequest
				return req.Nonce
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			c := idp.discover(t)
			req, err := c.Start()
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			nonce := tc.setup(t, idp, req)

			_, err = c.Exchange(context.Background(), "auth-code", req.CodeVerifier, nonce)
			if err == nil {
				t.Fatal("Exchange accepted an invalid assertion")
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want it to wrap ErrInvalidToken", err)
			}
		})
	}
}

// TestExchangeDistinguishesProviderFaults: a 5xx from the IdP is not the user
// failing to authenticate, and must not be reported as one — otherwise an IdP
// outage looks like every password suddenly being wrong.
func TestExchangeDistinguishesProviderFaults(t *testing.T) {
	idp := newFakeIdP(t)
	c := idp.discover(t)
	req, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	idp.tokenStatus = http.StatusInternalServerError

	_, err = c.Exchange(context.Background(), "auth-code", req.CodeVerifier, req.Nonce)
	if err == nil {
		t.Fatal("Exchange succeeded against a broken IdP")
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Errorf("provider fault reported as an invalid token: %v", err)
	}
}

func TestExchangeRequiresCompleteFlowState(t *testing.T) {
	idp := newFakeIdP(t)
	c := idp.discover(t)
	for _, tc := range []struct{ name, code, verifier, nonce string }{
		{"no code", "", "v", "n"},
		{"no verifier", "c", "", "n"},
		{"no nonce", "c", "v", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Exchange(context.Background(), tc.code, tc.verifier, tc.nonce); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// TestExchangeRefetchesJWKSOnRotation: when the IdP rotates its signing key,
// the first token bearing the new kid must trigger one refetch and then verify,
// rather than locking every user out until the process restarts.
func TestExchangeRefetchesJWKSOnRotation(t *testing.T) {
	idp := newFakeIdP(t)
	clock := time.Now().UTC()
	cfg := idp.config()
	cfg.Now = func() time.Time { return clock }
	c, err := Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	req, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	idp.nonce = req.Nonce

	// Rotate: new key, new kid, published at the JWKS endpoint.
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp.key, idp.kid = rotated, "idp-key-2"

	// Inside the throttle window the stale key set is kept, so the token does
	// not verify — the deliberate cost of not letting junk kids drive traffic
	// at the IdP.
	if _, err := c.Exchange(context.Background(), "auth-code", req.CodeVerifier, req.Nonce); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("inside the refetch throttle: error = %v, want ErrInvalidToken", err)
	}

	clock = clock.Add(2 * minJWKSRefetch)
	if _, err := c.Exchange(context.Background(), "auth-code", req.CodeVerifier, req.Nonce); err != nil {
		t.Fatalf("after the throttle window, Exchange: %v", err)
	}
}

func TestAudienceAcceptsBothJSONShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"single string", `"one"`, 1},
		{"array", `["one","two"]`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a audience
			if err := json.Unmarshal([]byte(tc.raw), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(a) != tc.want || !a.contains("one") {
				t.Errorf("audience = %v", a)
			}
		})
	}

	var a audience
	if err := json.Unmarshal([]byte(`{"aud":"one"}`), &a); err == nil {
		t.Error("audience accepted an object")
	}
}

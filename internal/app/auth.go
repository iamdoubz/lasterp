// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// errNoBearer is returned when a request carries no usable bearer token. The
// gateway maps any Authenticator error to a bare 401 (no detail leaked), so the
// specific reason stays server-side.
var errNoBearer = errors.New("app: missing or malformed bearer token")

// sessionAuthenticator resolves a request to its actor + tenant from an opaque
// session token (kernel/identity). This is the gateway's authn seam wired to the
// real session store (WP-1.4b); session issuance lands in sessions.go (WP-1.5),
// OIDC in WP-1.9. actor.TenantID == tenant by construction, so the gateway's
// tenant-mismatch guard always passes.
func sessionAuthenticator(db *storage.DB) api.Authenticator {
	return api.AuthenticatorFunc(func(r *http.Request) (authz.Actor, tenancy.ID, error) {
		tok := presentedToken(r)
		if tok == "" {
			return authz.Actor{}, "", errNoBearer
		}
		s, err := identity.ValidateSession(r.Context(), db, tok)
		if err != nil {
			// A wiped device is a decision, not an outage. It must be checked
			// before the fallthrough below, which would otherwise wrap it as
			// ErrAuthUnavailable and answer 503 — telling the device to retry
			// the very request that was supposed to end it (INV-D1).
			if errors.Is(err, identity.ErrDeviceWiped) {
				return authz.Actor{}, "", fmt.Errorf("%w: %w", api.ErrDeviceWiped, err)
			}
			// ValidateSession already separates "no such session" from "the
			// database did not answer" (ErrSessionInvalid vs a wrapped driver
			// error); this is where that distinction has to survive, because
			// the gateway turns everything else into a 401 and a 401 makes
			// clients discard credentials and queued work.
			if !errors.Is(err, identity.ErrSessionInvalid) {
				return authz.Actor{}, "", fmt.Errorf("%w: %w", api.ErrAuthUnavailable, err)
			}
			return authz.Actor{}, "", err
		}
		return authz.Actor{TenantID: s.TenantID, UserID: s.UserID}, s.TenantID, nil
	})
}

// presentedToken extracts the session token from either transport: the
// Authorization header (API, MCP, CLI — the original and still-primary form) or
// the HttpOnly session cookie the browser client uses (WP-1.5-decisions.md §5).
// The header wins when both are present, so an explicit credential is never
// silently overridden by an ambient one.
func presentedToken(r *http.Request) string {
	if tok, ok := bearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return cookieValue(r, cookieSession)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header (scheme is case-insensitive per RFC 7235).
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(scheme):])
	return tok, tok != ""
}

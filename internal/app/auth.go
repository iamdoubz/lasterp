// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
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
// bearer session token (kernel/identity). This is the gateway's authn seam
// wired to the real session store (WP-1.4b); HTTP login / session issuance
// (password/TOTP, OIDC) is WP-1.5 / WP-1.9. actor.TenantID == tenant by
// construction, so the gateway's tenant-mismatch guard always passes.
func sessionAuthenticator(db *storage.DB) api.Authenticator {
	return api.AuthenticatorFunc(func(r *http.Request) (authz.Actor, tenancy.ID, error) {
		tok, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			return authz.Actor{}, "", errNoBearer
		}
		s, err := identity.ValidateSession(r.Context(), db, tok)
		if err != nil {
			return authz.Actor{}, "", err
		}
		return authz.Actor{TenantID: s.TenantID, UserID: s.UserID}, s.TenantID, nil
	})
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

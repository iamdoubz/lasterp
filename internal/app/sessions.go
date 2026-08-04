// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Cookie names for the browser session. Tokens live in HttpOnly cookies and are
// never returned in a response body: the metadata renderer displays
// tenant-authored strings, so an XSS anywhere in the client must not be able to
// read a credential (WP-1.5-decisions.md §5). API and MCP clients keep using
// the Authorization: Bearer header, which is unchanged.
const (
	cookieSession = "lasterp_session"
	cookieRefresh = "lasterp_refresh"
	cookieDevice  = "lasterp_device"
)

// refreshPath scopes the refresh cookie to the one route that consumes it, so
// it is not attached to every ordinary API call.
const refreshPath = "/api/v1/sessions/refresh"

// sessionActions are the authentication routes. Login and refresh are the only
// public routes in the product — see WP-1.5-decisions.md §4 for why the
// exemption exists and kernel/api.Action.Public for what it does and does not
// waive. Logout is authenticated: it revokes the caller's own session.
func sessionActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "POST", Path: "/api/v1/sessions", Public: true,
			Summary: "Log in and start a session", Handler: login(db)},
		{Method: "POST", Path: refreshPath, Public: true,
			Summary: "Refresh the current session's access token", Handler: refresh(db)},
		{Method: "DELETE", Path: "/api/v1/sessions/current", Object: "",
			Summary: "Log out and revoke the current session", Handler: logout(db)},
	}
}

// loginReq is the login payload. Tenant is explicit because email is unique
// only per tenant (idx_users_tenant_email) — the same address can be a user in
// two tenants by design, so there is nothing to infer it from. A deployment
// fronting LastERP with per-tenant subdomains can fill it in at the edge.
type loginReq struct {
	Tenant   string `json:"tenant"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`

	// RecoveryCode stands in for TOTP when the authenticator is gone. A
	// separate field rather than a value told apart from TOTP by shape (six
	// digits vs. dashed base32): an authentication path should not contain a
	// guess (WP-1.12-decisions.md §5). Adding a field to an already-allowlisted
	// route does not touch the public-route allowlist.
	RecoveryCode string `json:"recovery_code"`
}

// sessionResp is what a successful login/refresh returns. It deliberately
// carries no token — the tokens are in HttpOnly cookies. Expiry is exposed so
// the client can schedule a refresh without decoding a credential.
type sessionResp struct {
	UserID    string    `json:"user_id"`
	Tenant    string    `json:"tenant"`
	ExpiresAt time.Time `json:"expires_at"`
}

func login(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ tenancy.ID) {
		var req loginReq
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Tenant == "" || req.Email == "" || req.Password == "" {
			unauthorized(w, r)
			return
		}
		tenant := tenancy.ID(req.Tenant)

		// The password rules — unknown user, wrong password, TOTP validation and
		// step burning, and the timing equalization that makes the first two
		// indistinguishable — all live in the provider. Until WP-1.10 they lived
		// here *as well*, in a second copy that had drifted: the provider was
		// missing the equalizer, and nothing outside its own unit tests called
		// it. One authentication decision, one implementation.
		provider := &identity.PasswordTOTPProvider{DB: db}
		user, err := provider.Authenticate(r.Context(), tenant, identity.Credentials{
			Email: req.Email, Password: req.Password,
			TOTPCode: req.TOTP, RecoveryCode: req.RecoveryCode,
		})
		if errors.Is(err, identity.ErrInvalidCredentials) {
			unauthorized(w, r)
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}

		device := deviceID(r)
		issued, err := identity.IssueSession(r.Context(), db, tenant, user, device)
		if err != nil {
			fail(w, r, err)
			return
		}
		if err := identity.AuditSession(r.Context(), db, tenant, user, issued.ID, identity.AuditLogin); err != nil {
			fail(w, r, err)
			return
		}
		// A login that spent a recovery code is audited too, as a separate
		// event — it means the user has lost their authenticator. That record
		// is written by the provider, which is the only layer that knows a code
		// was actually consumed rather than merely offered.

		setSessionCookies(w, issued, device)
		writeJSON(w, http.StatusOK, sessionResp{
			UserID: string(user), Tenant: string(tenant), ExpiresAt: issued.ExpiresAt,
		})
	}
}

func refresh(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ tenancy.ID) {
		token := cookieValue(r, cookieRefresh)
		device := cookieValue(r, cookieDevice)
		if token == "" || device == "" {
			unauthorized(w, r)
			return
		}
		issued, err := identity.RefreshSession(r.Context(), db, token, device)
		if err != nil {
			// A wiped device is told so, here as everywhere else: an expiring
			// client reaches refresh before it reaches anything else, so this
			// is often where a wipe is first delivered. Everything else —
			// device mismatch, unknown token — is just "no" (WP-2.5 §3).
			if errors.Is(err, identity.ErrDeviceWiped) {
				deviceWiped(w, r)
				return
			}
			unauthorized(w, r)
			return
		}
		tenant, user, err := identity.SessionOwner(r.Context(), db, issued.ID)
		if err != nil {
			unauthorized(w, r)
			return
		}
		setSessionCookies(w, issued, device)
		writeJSON(w, http.StatusOK, sessionResp{
			UserID: string(user), Tenant: string(tenant), ExpiresAt: issued.ExpiresAt,
		})
	}
}

func logout(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		// The caller is authenticated, so the session behind the presented
		// token is the only one they can revoke — no id is accepted from the
		// request, which would let one user revoke another's session.
		s, err := identity.ValidateSession(r.Context(), db, presentedToken(r))
		if err != nil {
			unauthorized(w, r)
			return
		}
		if err := identity.RevokeSession(r.Context(), db, s.ID); err != nil {
			fail(w, r, err)
			return
		}
		if err := identity.AuditSession(r.Context(), db, tenant, s.UserID, s.ID, identity.AuditLogout); err != nil {
			fail(w, r, err)
			return
		}
		clearSessionCookies(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// setSessionCookies writes the access, refresh and device cookies. All three are
// HttpOnly (no JS access), Secure, and SameSite=Strict — the last is half of the
// CSRF story; the other half is ADR-009's mandatory Idempotency-Key header on
// writes, which a cross-origin form post cannot set.
func setSessionCookies(w http.ResponseWriter, issued *identity.IssuedSession, device string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieSession, Value: issued.Token, Path: "/",
		Expires: issued.ExpiresAt, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: cookieRefresh, Value: issued.RefreshToken, Path: refreshPath,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	// The device id is what binds a refresh token to the browser that got it
	// (identity.RefreshSession rejects a mismatch), so it must outlive the
	// access token and be sent to the refresh route.
	http.SetCookie(w, &http.Cookie{
		Name: cookieDevice, Value: device, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookies(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{
		{cookieSession, "/"}, {cookieRefresh, refreshPath}, {cookieDevice, "/"},
	} {
		http.SetCookie(w, &http.Cookie{
			Name: c.name, Value: "", Path: c.path, MaxAge: -1,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
}

// deviceID reuses the browser's existing device cookie when it has one, so
// logging in again from the same browser keeps one device identity, and mints a
// fresh one otherwise. It is an opaque correlator, not a credential: knowing it
// grants nothing without the refresh token it is checked against.
func deviceID(r *http.Request) string {
	if existing := cookieValue(r, cookieDevice); existing != "" {
		return existing
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a recoverable condition; a constant here
		// would silently collapse every browser onto one device identity.
		panic("app: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, http.StatusUnauthorized, "invalid credentials", "", r.URL.Path)
}

// deviceWiped is unauthorized with a reason attached — the same typed problem
// the gateway emits for authenticated routes, so a client has one thing to
// recognise no matter which route delivered it (WP-2.5, INV-D1).
func deviceWiped(w http.ResponseWriter, r *http.Request) {
	writeProblemTyped(w, api.ProblemDeviceWiped, http.StatusUnauthorized, "device has been wiped",
		"this device was remotely wiped by an administrator; its local data must be destroyed",
		r.URL.Path)
}

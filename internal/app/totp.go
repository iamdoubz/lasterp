// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"net/http"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// totpActions are the self-service second-factor routes (WP-1.12).
//
// "me" rather than "users/me": users is a resource segment a future metadata
// object could plausibly claim, and me is one nothing can. Object is "" —
// these are account operations, not capability-gated module objects, exactly
// like DELETE /api/v1/sessions/current.
//
// No route accepts a user id. Enrollment and disablement act only on the
// caller's own account, for the same reason logout accepts no session id: a
// path parameter here would be an authorization decision made by a URL
// (INV-T2 — no write path executes without an authenticated principal and an
// authorization decision).
//
// All three writes set CarriesCredentials: every one of them has a secret in
// its request body, its response body, or both, and idempotency_keys has no TTL
// and no cleanup (decisions §9).
func totpActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/me/totp", Object: "",
			Summary: "Get the caller's second-factor status", Handler: totpStatus(db)},
		{Method: "POST", Path: "/api/v1/me/totp/enroll", Object: "", Write: true, CarriesCredentials: true,
			Summary: "Start second-factor enrollment", Handler: totpEnroll(db)},
		{Method: "POST", Path: "/api/v1/me/totp/confirm", Object: "", Write: true, CarriesCredentials: true,
			Summary: "Confirm and enable the second factor", Handler: totpConfirm(db)},
		{Method: "POST", Path: "/api/v1/me/totp/disable", Object: "", Write: true, CarriesCredentials: true,
			Summary: "Disable the second factor", Handler: totpDisable(db)},
	}
}

type totpStatusResp struct {
	Enabled                bool `json:"enabled"`
	Pending                bool `json:"pending"`
	RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
}

type totpEnrollReq struct {
	Password string `json:"password"`
}

type totpEnrollResp struct {
	OTPAuthURI string `json:"otpauth_uri"`
	Secret     string `json:"secret"`
	QRPNG      string `json:"qr_png"`
}

type totpConfirmReq struct {
	Code string `json:"code"`
}

type totpConfirmResp struct {
	Enabled       bool     `json:"enabled"`
	RecoveryCodes []string `json:"recovery_codes"`
}

type totpDisableReq struct {
	Password     string `json:"password"`
	TOTP         string `json:"totp"`
	RecoveryCode string `json:"recovery_code"`
}

func totpStatus(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		user, ok := callerUser(w, r, db, tenant)
		if !ok {
			return
		}
		remaining, err := identity.CountUnusedRecoveryCodes(r.Context(), db, tenant, user.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, totpStatusResp{
			Enabled: user.TOTPEnabled,
			// A stored secret that is not enabled *is* a pending enrollment
			// (decisions §1) — there is no separate state to read.
			Pending:                !user.TOTPEnabled && user.TOTPSecret != "",
			RecoveryCodesRemaining: remaining,
		})
	}
}

// totpEnroll mints a pending secret and returns what the user needs to add it
// to an authenticator.
//
// It requires the password. Decisions §4 originally exempted enrollment on the
// grounds that enabling a factor is a security *upgrade* and friction on an
// upgrade suppresses adoption. That reasoning misses an absorbing state: an
// attacker holding only a session enrolls their own authenticator, keeps the
// recovery codes, and the legitimate owner — who still knows the password —
// can no longer disable it, because disable demands a second factor they do not
// have, and no administrator reset exists. Password-only has no such state, so
// "no worse than the status quo" was wrong. The account is password-only at
// this point, so asking for the password is asking for the only factor there
// is; it costs one field and closes a session-theft path into permanent
// account loss.
func totpEnroll(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req totpEnrollReq
		if !decodeJSON(w, r, &req) {
			return
		}
		user, ok := callerUser(w, r, db, tenant)
		if !ok {
			return
		}
		userID := user.ID

		// The two refusals that are not about credentials come first, so a
		// caller is not asked to prove who they are before being told the
		// request was never going to work — and so "already enabled" stays the
		// 409 that tells them to disable first (decisions §8). Neither leaks
		// anything: GET /api/v1/me/totp reports both to the same caller.
		switch {
		case user.TOTPEnabled:
			writeProblem(w, http.StatusConflict, "TOTP is already enabled",
				"disable the current second factor before enrolling a new one", r.URL.Path)
			return
		case user.PasswordHash == "":
			writeProblem(w, http.StatusUnprocessableEntity, "TOTP enrollment requires a password",
				"this account authenticates through an identity provider; enable MFA there", r.URL.Path)
			return
		}

		// The account has no second factor yet, so this is a password check —
		// but it runs through the same provider decision as everything else, so
		// there is still one implementation of it.
		provider := &identity.PasswordTOTPProvider{DB: db}
		if err := provider.Reauthenticate(r.Context(), tenant, userID, identity.Credentials{
			Password: req.Password,
		}); errors.Is(err, identity.ErrInvalidCredentials) {
			unauthorized(w, r)
			return
		} else if err != nil {
			fail(w, r, err)
			return
		}

		// StartTOTPEnrollment re-checks both conditions above under its own
		// read: the checks here choose the status code, they are not the guard.
		secret, err := identity.StartTOTPEnrollment(r.Context(), db, tenant, userID)
		switch {
		case errors.Is(err, identity.ErrTOTPAlreadyEnabled):
			writeProblem(w, http.StatusConflict, "TOTP is already enabled",
				"disable the current second factor before enrolling a new one", r.URL.Path)
			return
		case errors.Is(err, identity.ErrPasswordRequired):
			writeProblem(w, http.StatusUnprocessableEntity, "TOTP enrollment requires a password",
				"this account authenticates through an identity provider; enable MFA there", r.URL.Path)
			return
		case err != nil:
			fail(w, r, err)
			return
		}

		uri := identity.TOTPURI(tenant, user.Email, secret)
		png, err := identity.TOTPQRDataURI(uri)
		if err != nil {
			fail(w, r, err)
			return
		}
		if err := identity.AuditAccountSecurity(r.Context(), db, tenant, userID, identity.AuditTOTPEnrollStarted); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, totpEnrollResp{OTPAuthURI: uri, Secret: secret, QRPNG: png})
	}
}

// totpConfirm validates a code against the pending secret, enables the factor
// and issues the recovery codes — the only time they are ever readable.
func totpConfirm(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req totpConfirmReq
		if !decodeJSON(w, r, &req) {
			return
		}
		actor, err := authz.ActorFromContext(r.Context())
		if err != nil {
			fail(w, r, err)
			return
		}
		userID := identity.UserID(actor.UserID)

		// Enabling the factor and minting its recovery codes are one
		// transaction inside the kernel, so there is no state where the account
		// has a second factor and the user never saw a way back in.
		codes, err := identity.ConfirmTOTPEnrollment(r.Context(), db, tenant, userID, req.Code, time.Now().UTC())
		switch {
		case errors.Is(err, identity.ErrInvalidCredentials):
			unauthorized(w, r)
			return
		case errors.Is(err, identity.ErrTOTPAlreadyEnabled):
			writeProblem(w, http.StatusConflict, "TOTP is already enabled", "", r.URL.Path)
			return
		case err != nil:
			fail(w, r, err)
			return
		}

		if err := identity.AuditAccountSecurity(r.Context(), db, tenant, userID, identity.AuditTOTPEnabled); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, totpConfirmResp{Enabled: true, RecoveryCodes: codes})
	}
}

// totpDisable removes the second factor. It demands the password *and* a
// second factor, because the threat is a session an attacker holds rather than
// a password they know: an unattended logged-in browser must not be able to
// strip MFA off the account.
//
// A recovery code counts as the second factor. Requiring a live TOTP code would
// deadlock the exact user who most needs this — someone whose authenticator
// device is gone — which is what recovery codes are for (decisions §4).
func totpDisable(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req totpDisableReq
		if !decodeJSON(w, r, &req) {
			return
		}
		user, ok := callerUser(w, r, db, tenant)
		if !ok {
			return
		}
		userID := user.ID

		// Nothing enabled is a 409, not a silent success. Disabling a factor
		// that is not on would write a `totp.disabled` audit row for a
		// transition that did not happen (INV-T4 is about attributable
		// mutations, not imagined ones) — and, because Reauthenticate demands a
		// second factor only when one exists, it would let a password-holder
		// with a session quietly discard someone's pending enrollment.
		if !user.TOTPEnabled {
			writeProblem(w, http.StatusConflict, "TOTP is not enabled",
				"there is no second factor on this account to disable", r.URL.Path)
			return
		}

		// Reauthenticate runs the same decision the login route runs, so there
		// is one implementation of "is this person who they claim to be". The
		// second factor is not optional here: TOTP is enabled — checked just
		// above — and verify demands one whenever it is.
		provider := &identity.PasswordTOTPProvider{DB: db}
		err := provider.Reauthenticate(r.Context(), tenant, userID, identity.Credentials{
			Password: req.Password, TOTPCode: req.TOTP, RecoveryCode: req.RecoveryCode,
		})
		if errors.Is(err, identity.ErrInvalidCredentials) {
			unauthorized(w, r)
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}

		if err := identity.DisableTOTP(r.Context(), db, tenant, userID); err != nil {
			fail(w, r, err)
			return
		}
		if err := identity.AuditAccountSecurity(r.Context(), db, tenant, userID, identity.AuditTOTPDisabled); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// callerUser resolves the authenticated actor to their user row.
func callerUser(w http.ResponseWriter, r *http.Request, db *storage.DB, tenant tenancy.ID) (*identity.User, bool) {
	actor, err := authz.ActorFromContext(r.Context())
	if err != nil {
		fail(w, r, err)
		return nil, false
	}
	user, err := identity.GetUserByID(r.Context(), db, tenant, identity.UserID(actor.UserID))
	if err != nil {
		fail(w, r, err)
		return nil, false
	}
	return user, true
}

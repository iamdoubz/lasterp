// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"errors"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Credentials is provider-specific. PasswordTOTPProvider reads Email,
// Password, and — if the account has TOTP enabled — TOTPCode. OIDCProvider
// reads Code, CodeVerifier and Nonce, which the caller carries across the
// redirect from the AuthRequest that started the flow.
type Credentials struct {
	Email    string
	Password string
	TOTPCode string

	// RecoveryCode is a single-use code standing in for TOTPCode when the
	// authenticator is unavailable. It is a separate field rather than a
	// value sniffed out of TOTPCode by shape: an authentication path should
	// not contain a guess, and the audit trail can then tell the two apart
	// (WP-1.12-decisions.md §5).
	RecoveryCode string

	// Code is the authorization code returned by the IdP; CodeVerifier is the
	// PKCE verifier whose challenge was sent with the authorization request;
	// Nonce is the value the resulting ID token must carry to prove it belongs
	// to this login attempt and not a replayed one.
	Code         string
	CodeVerifier string
	Nonce        string
}

// ErrInvalidCredentials is returned for any authentication failure.
// Deliberately undifferentiated (wrong email vs. wrong password vs.
// missing TOTP code) so a failed attempt can't be used to enumerate
// valid emails or account configuration.
var ErrInvalidCredentials = errors.New("identity: invalid credentials")

// AuthProvider authenticates credentials into a UserID. Two implementations
// exist: PasswordTOTPProvider below, and OIDCProvider (oidc.go, WP-1.9 — the
// follow-up WP-0.3 deferred until ADR-019 settled the JOSE question).
type AuthProvider interface {
	Authenticate(ctx context.Context, tenant tenancy.ID, creds Credentials) (UserID, error)
}

// PasswordTOTPProvider is the built-in email+password(+TOTP) provider.
type PasswordTOTPProvider struct {
	DB *storage.DB
}

func (p *PasswordTOTPProvider) Authenticate(ctx context.Context, tenant tenancy.ID, creds Credentials) (UserID, error) {
	u, err := GetUserByEmail(ctx, p.DB, tenant, creds.Email)
	if errors.Is(err, ErrNotFound) {
		// Spend the same time a real verification would, or the response time
		// alone answers "is this an account here?" (see EqualizeVerifyTiming).
		EqualizeVerifyTiming(creds.Password)
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", err
	}
	return p.verify(ctx, tenant, u, creds)
}

// Reauthenticate re-runs the same decision Authenticate runs, against a user
// id instead of an email. It exists for the operations that a valid session is
// not sufficient to authorize — today, disabling TOTP: the threat is a session
// an attacker holds, not a password they know, so an unattended logged-in
// browser must not be able to strip MFA off the account (decisions §4).
//
// It shares verify with Authenticate, so there is still exactly one
// implementation of "is this person who they claim to be".
func (p *PasswordTOTPProvider) Reauthenticate(ctx context.Context, tenant tenancy.ID, id UserID, creds Credentials) error {
	u, err := GetUserByID(ctx, p.DB, tenant, id)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	_, err = p.verify(ctx, tenant, u, creds)
	return err
}

// verify is the one place password and second factor are checked. A second
// factor is demanded exactly when the account has one enabled, so a disable —
// which by definition runs on an account with TOTP on — can never skip it.
func (p *PasswordTOTPProvider) verify(ctx context.Context, tenant tenancy.ID, u *User, creds Credentials) (UserID, error) {
	if !VerifyPassword(u.PasswordHash, creds.Password) {
		return "", ErrInvalidCredentials
	}
	if !u.TOTPEnabled {
		return u.ID, nil
	}

	now := time.Now().UTC()
	// Locked: refuse without evaluating anything. Same undifferentiated 401 as
	// every other failure — a distinct "you are locked out" would be an oracle
	// telling an attacker their password guess was right.
	if u.TOTPLockedAt(now) {
		return "", ErrInvalidCredentials
	}

	ok, err := p.secondFactor(ctx, tenant, u, creds, now)
	if err != nil {
		return "", err
	}
	if !ok {
		if err := recordTOTPFailure(ctx, p.DB, tenant, u, now); err != nil {
			return "", err
		}
		return "", ErrInvalidCredentials
	}
	if err := clearTOTPFailures(ctx, p.DB, tenant, u.ID); err != nil {
		return "", err
	}
	return u.ID, nil
}

// secondFactor checks whichever of the two the caller presented. A recovery
// code is offered explicitly, so there is no ambiguity about which was meant.
func (p *PasswordTOTPProvider) secondFactor(ctx context.Context, tenant tenancy.ID, u *User, creds Credentials, now time.Time) (bool, error) {
	if creds.RecoveryCode != "" {
		consumed, err := ConsumeRecoveryCode(ctx, p.DB, tenant, u.ID, creds.RecoveryCode)
		if err != nil || !consumed {
			return false, err
		}
		// Audited here rather than in the handler, for the same reason
		// AuditSession lives in the kernel: this is the only place that knows a
		// code was actually spent. A handler inferring it from "the request had
		// a recovery_code field" would log the event for an account with no
		// second factor at all, where the field is never even read — an audit
		// trail that reports things that did not happen is worse than none.
		if err := AuditAccountSecurity(ctx, p.DB, tenant, u.ID, AuditRecoveryUsed); err != nil {
			return false, err
		}
		return true, nil
	}
	ok, counter, err := ValidateTOTP(u.TOTPSecret, creds.TOTPCode, now, u.TOTPLastCounter)
	if err != nil || !ok {
		return false, err
	}
	// Burn the step before reporting success, or the code stays live for the
	// rest of its window.
	if err := SetTOTPLastCounter(ctx, p.DB, tenant, u.ID, counter); err != nil {
		return false, err
	}
	return true, nil
}

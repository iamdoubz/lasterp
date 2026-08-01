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
	if !VerifyPassword(u.PasswordHash, creds.Password) {
		return "", ErrInvalidCredentials
	}
	if u.TOTPEnabled {
		ok, counter, err := ValidateTOTP(u.TOTPSecret, creds.TOTPCode, time.Now().UTC(), u.TOTPLastCounter)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrInvalidCredentials
		}
		if err := SetTOTPLastCounter(ctx, p.DB, tenant, u.ID, counter); err != nil {
			return "", err
		}
	}
	return u.ID, nil
}

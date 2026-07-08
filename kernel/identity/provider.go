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
// Password, and — if the account has TOTP enabled — TOTPCode.
type Credentials struct {
	Email    string
	Password string
	TOTPCode string
}

// ErrInvalidCredentials is returned for any authentication failure.
// Deliberately undifferentiated (wrong email vs. wrong password vs.
// missing TOTP code) so a failed attempt can't be used to enumerate
// valid emails or account configuration.
var ErrInvalidCredentials = errors.New("identity: invalid credentials")

// AuthProvider authenticates credentials into a UserID. OIDC is a second
// implementation of this interface, deferred to a follow-up WP once a JOSE
// library is picked under its own ADR (docs/notes/WP-0.3-decisions.md).
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

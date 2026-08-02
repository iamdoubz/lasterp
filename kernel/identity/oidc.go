// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/oidc"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// AuditLoginOIDC is the audit_log action for a login through an external
// identity provider. It is distinct from AuditLogin so the trail says how a
// principal got in, not merely that they did — the two paths have different
// revocation stories (WP-1.9-decisions.md, "Deferred").
const AuditLoginOIDC = "login.oidc"

// OIDCProvider authenticates an OpenID Connect authorization code into a local
// UserID. It is the second implementation of AuthProvider, deferred from WP-0.3
// pending ADR-019.
//
// It authenticates only: the IdP says who someone is, and LastERP decides what
// they may do (kernel/authz). Group and role claims are deliberately not read —
// an IdP administrator must not be able to grant permissions inside a tenant by
// editing a directory.
type OIDCProvider struct {
	DB     *storage.DB
	Client *oidc.Client
}

// Authenticate redeems creds.Code and resolves the resulting assertion to a
// local user. Credentials is provider-specific by design (see its doc): this
// provider reads Code, CodeVerifier and Nonce, all three of which come from the
// AuthRequest that started the flow.
//
// Every rejection is ErrInvalidCredentials, the same undifferentiated error the
// password path returns, so the callback cannot be used to learn which subjects
// or emails exist in a tenant. A transport or provider fault is returned as
// itself: an IdP outage is not a failed login and must not be logged as one.
func (p *OIDCProvider) Authenticate(ctx context.Context, tenant tenancy.ID, creds Credentials) (UserID, error) {
	claims, err := p.Client.Exchange(ctx, creds.Code, creds.CodeVerifier, creds.Nonce)
	if errors.Is(err, oidc.ErrInvalidToken) {
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", err
	}
	return ResolveOIDCUser(ctx, p.DB, tenant, claims)
}

// ResolveOIDCUser maps validated claims to a local user, linking on first use.
// It never creates one: an IdP cannot provision principals into a LastERP
// tenant (WP-1.9-decisions.md §3).
//
//  1. a user already bound to (issuer, subject) — the steady state;
//  2. otherwise, and only if the IdP asserts the email is verified, a user with
//     that email, which is then bound to the subject for next time;
//  3. otherwise no.
//
// Step 2 is the account-takeover guard: without the email_verified check, an
// IdP that lets a user type any address would let them claim any local account.
// Once bound, matching is by subject, so a later email change at the IdP
// neither breaks the link nor lets someone else inherit it.
func ResolveOIDCUser(ctx context.Context, db *storage.DB, tenant tenancy.ID, claims *oidc.Claims) (UserID, error) {
	if tenant == "" || claims == nil || claims.Issuer == "" || claims.Subject == "" {
		return "", ErrInvalidCredentials
	}

	u, err := GetUserByOIDCSubject(ctx, db, tenant, claims.Issuer, claims.Subject)
	if err == nil {
		return u.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}

	if !claims.EmailVerified || claims.Email == "" {
		return "", ErrInvalidCredentials
	}
	u, err = GetUserByEmail(ctx, db, tenant, claims.Email)
	if errors.Is(err, ErrNotFound) {
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", err
	}
	if err := LinkOIDCIdentity(ctx, db, tenant, u.ID, claims.Issuer, claims.Subject); err != nil {
		if errors.Is(err, ErrAlreadyLinked) {
			return "", ErrInvalidCredentials
		}
		return "", err
	}
	return u.ID, nil
}

// ErrAlreadyLinked is returned when a user is already bound to a different
// external identity.
var ErrAlreadyLinked = errors.New("identity: user is already linked to an external identity")

// GetUserByOIDCSubject looks up the user bound to (issuer, subject) within
// tenant. The tenant filter is not decoration: the same corporate IdP legitimately
// backs several tenants in one deployment, and a subject linked in one must not
// authenticate into another (INV-T1).
func GetUserByOIDCSubject(ctx context.Context, db *storage.DB, tenant tenancy.ID, issuer, subject string) (*User, error) {
	if tenant == "" || issuer == "" || subject == "" {
		return nil, ErrNotFound
	}
	var u *User
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT `+userColumns+`
			FROM users WHERE tenant_id = ? AND oidc_issuer = ? AND oidc_subject = ?`),
			string(tenant), issuer, subject)
		got, err := scanUser(row)
		if err != nil {
			return err
		}
		u = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// LinkOIDCIdentity binds a user to an external identity, once. The
// oidc_subject IS NULL guard makes the link write-once: an IdP cannot re-point
// an existing account at a new subject by reusing its email address, which
// would otherwise be a takeover path through a directory rename.
func LinkOIDCIdentity(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, issuer, subject string) error {
	if tenant == "" || id == "" || issuer == "" || subject == "" {
		return errors.New("identity: tenant, user id, issuer and subject are required to link an identity")
	}
	var affected int64
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET oidc_issuer = ?, oidc_subject = ?
			WHERE tenant_id = ? AND id = ? AND oidc_subject IS NULL`),
			issuer, subject, string(tenant), string(id))
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: link OIDC identity: %w", err)
	}
	if affected == 0 {
		return ErrAlreadyLinked
	}
	return nil
}

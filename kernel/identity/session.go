// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// SessionTTL is how long an issued session's access token is valid.
const SessionTTL = 24 * time.Hour

// SessionID identifies a session row.
type SessionID string

// ErrSessionInvalid covers any reason a token doesn't grant access:
// unknown, expired, or revoked. Deliberately undifferentiated so callers
// can't distinguish "wrong token" from "right token, revoked" (that
// distinction is an oracle an attacker could use).
var ErrSessionInvalid = errors.New("identity: session invalid")

// ErrDeviceMismatch is returned by RefreshSession when the refresh token
// is presented from a device other than the one it was issued to
// (08-SECURITY-MULTITENANCY.md: "refresh bound to device").
var ErrDeviceMismatch = errors.New("identity: refresh token device mismatch")

// Session is a tenant/user-bound bearer-token grant. Only token hashes are
// stored; the plaintext token exists solely in IssuedSession, returned
// once at issuance/refresh time.
type Session struct {
	ID        SessionID
	TenantID  tenancy.ID
	UserID    UserID
	DeviceID  string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IssuedSession carries the plaintext bearer tokens back to the caller at
// issuance time. Neither token is retrievable again once this value is
// discarded — session.go stores only their SHA-256 hashes.
type IssuedSession struct {
	ID           SessionID
	Token        string
	RefreshToken string
	ExpiresAt    time.Time
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueSession creates a new session for user on device, scoped to
// tenant. tenant, user and device must all be non-empty (INV-T4: every
// session is attributable to a principal and a device).
func IssueSession(ctx context.Context, db *storage.DB, tenant tenancy.ID, user UserID, deviceID string) (*IssuedSession, error) {
	if tenant == "" || user == "" || deviceID == "" {
		return nil, errors.New("identity: tenant, user and device are required to issue a session")
	}
	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate token: %w", err)
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate refresh token: %w", err)
	}

	now := time.Now().UTC()
	issued := &IssuedSession{
		ID:           SessionID(idgen.New()),
		Token:        token,
		RefreshToken: refresh,
		ExpiresAt:    now.Add(SessionTTL),
	}

	_, err = db.ExecContext(ctx, db.Rebind(`
		INSERT INTO sessions (id, tenant_id, user_id, device_id, token_hash, refresh_token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		string(issued.ID), string(tenant), string(user), deviceID,
		hashToken(token), hashToken(refresh), now, issued.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("identity: issue session: %w", err)
	}
	return issued, nil
}

// ValidateSession resolves a bearer token to its session, or
// ErrSessionInvalid if the token is unknown, expired, or revoked. This
// necessarily runs before tenant context exists — the token is what
// determines the tenant — which is why sessions is exempt from RLS
// (docs/notes/WP-0.3-decisions.md).
func ValidateSession(ctx context.Context, db *storage.DB, token string) (*Session, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, user_id, device_id, expires_at, revoked_at
		FROM sessions WHERE token_hash = ?`), hashToken(token))

	s, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	if s.RevokedAt != nil || time.Now().UTC().After(s.ExpiresAt) {
		return nil, ErrSessionInvalid
	}
	return s, nil
}

// RefreshSession issues a new access token for the session owning
// refreshToken, provided it is presented from the same deviceID it was
// issued to. The old access token keeps working until it naturally
// expires; refreshing does not revoke it.
func RefreshSession(ctx context.Context, db *storage.DB, refreshToken, deviceID string) (*IssuedSession, error) {
	if refreshToken == "" || deviceID == "" {
		return nil, ErrSessionInvalid
	}
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, user_id, device_id, expires_at, revoked_at
		FROM sessions WHERE refresh_token_hash = ?`), hashToken(refreshToken))

	s, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	if s.RevokedAt != nil {
		return nil, ErrSessionInvalid
	}
	if s.DeviceID != deviceID {
		return nil, ErrDeviceMismatch
	}

	newToken, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate token: %w", err)
	}
	expiresAt := time.Now().UTC().Add(SessionTTL)
	_, err = db.ExecContext(ctx, db.Rebind(`
		UPDATE sessions SET token_hash = ?, expires_at = ? WHERE id = ?`),
		hashToken(newToken), expiresAt, string(s.ID))
	if err != nil {
		return nil, fmt.Errorf("identity: refresh session: %w", err)
	}
	return &IssuedSession{ID: s.ID, Token: newToken, RefreshToken: refreshToken, ExpiresAt: expiresAt}, nil
}

// RevokeSession invalidates a session immediately.
func RevokeSession(ctx context.Context, db *storage.DB, id SessionID) error {
	_, err := db.ExecContext(ctx, db.Rebind(`
		UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), string(id))
	if err != nil {
		return fmt.Errorf("identity: revoke session: %w", err)
	}
	return nil
}

func scanSession(row *sql.Row) (*Session, error) {
	var s Session
	var idStr, tenantStr, userStr string
	var expiresAt storage.Time
	var revokedAt storage.NullTime
	err := row.Scan(&idStr, &tenantStr, &userStr, &s.DeviceID, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("identity: scan session: %w", err)
	}
	s.ID = SessionID(idStr)
	s.TenantID = tenancy.ID(tenantStr)
	s.UserID = UserID(userStr)
	s.ExpiresAt = expiresAt.Time
	if revokedAt.Valid {
		s.RevokedAt = &revokedAt.Time
	}
	return &s, nil
}

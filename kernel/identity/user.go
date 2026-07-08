// SPDX-License-Identifier: AGPL-3.0-only

// Package identity is the WP-0.3 kernel: users, sessions, password/TOTP
// authentication. Every query takes tenant explicitly and filters on it —
// defense in depth alongside Postgres RLS (INV-T1), and the only guard at
// all on SQLite, where RLS doesn't apply (ADR-005 solo-mode bypass).
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// UserID identifies a user within a tenant.
type UserID string

// ErrNotFound is returned by lookups that find no matching row.
var ErrNotFound = errors.New("identity: not found")

// User is a tenant-scoped principal.
type User struct {
	ID              UserID
	TenantID        tenancy.ID
	Email           string
	PasswordHash    string
	TOTPSecret      string
	TOTPEnabled     bool
	TOTPLastCounter *int64
	CreatedAt       time.Time
}

// CreateUser inserts a new user with a bcrypt password hash. tenant and
// email must be non-empty — INV-T2 requires every write to have an
// authorization-relevant scope, and an empty tenant would be a
// cross-tenant write by construction.
func CreateUser(ctx context.Context, db *storage.DB, tenant tenancy.ID, email, passwordHash string) (*User, error) {
	if tenant == "" || email == "" {
		return nil, errors.New("identity: tenant and email are required")
	}
	u := &User{
		ID:           UserID(uuid.NewString()),
		TenantID:     tenant,
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		INSERT INTO users (id, tenant_id, email, password_hash, totp_enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		string(u.ID), string(u.TenantID), u.Email, u.PasswordHash, false, u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("identity: create user: %w", err)
	}
	return u, nil
}

// GetUserByEmail looks up a user scoped to tenant. Returns ErrNotFound if
// absent or if tenant doesn't match (including another tenant's user with
// the same email — the (tenant_id, email) unique index permits reuse of
// an email across tenants by design).
func GetUserByEmail(ctx context.Context, db *storage.DB, tenant tenancy.ID, email string) (*User, error) {
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, email, password_hash, totp_secret, totp_enabled, totp_last_counter, created_at
		FROM users WHERE tenant_id = ? AND email = ?`), string(tenant), email)
	return scanUser(row)
}

// GetUserByID looks up a user scoped to tenant.
func GetUserByID(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) (*User, error) {
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, email, password_hash, totp_secret, totp_enabled, totp_last_counter, created_at
		FROM users WHERE tenant_id = ? AND id = ?`), string(tenant), string(id))
	return scanUser(row)
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var idStr, tenantStr string
	var totpSecret sql.NullString
	var totpLastCounter sql.NullInt64
	var createdAt storage.Time
	err := row.Scan(&idStr, &tenantStr, &u.Email, &u.PasswordHash, &totpSecret, &u.TOTPEnabled, &totpLastCounter, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("identity: scan user: %w", err)
	}
	u.ID = UserID(idStr)
	u.TenantID = tenancy.ID(tenantStr)
	u.TOTPSecret = totpSecret.String
	u.CreatedAt = createdAt.Time
	if totpLastCounter.Valid {
		u.TOTPLastCounter = &totpLastCounter.Int64
	}
	return &u, nil
}

// EnableTOTP stores secret on the user and marks TOTP enabled.
func EnableTOTP(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, secret string) error {
	if tenant == "" || id == "" {
		return errors.New("identity: tenant and user id are required")
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE tenant_id = ? AND id = ?`),
		secret, true, string(tenant), string(id))
	if err != nil {
		return fmt.Errorf("identity: enable TOTP: %w", err)
	}
	return nil
}

// SetTOTPLastCounter persists the last-consumed TOTP step, closing the
// replay window (see ValidateTOTP).
func SetTOTPLastCounter(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, counter int64) error {
	if tenant == "" || id == "" {
		return errors.New("identity: tenant and user id are required")
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		UPDATE users SET totp_last_counter = ? WHERE tenant_id = ? AND id = ?`),
		counter, string(tenant), string(id))
	if err != nil {
		return fmt.Errorf("identity: set TOTP last counter: %w", err)
	}
	return nil
}

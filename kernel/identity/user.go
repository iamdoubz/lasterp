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

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// UserID identifies a user within a tenant.
type UserID string

// ErrNotFound is returned by lookups that find no matching row.
var ErrNotFound = errors.New("identity: not found")

// User is a tenant-scoped principal.
//
// A pending TOTP enrollment is TOTPSecret set with TOTPEnabled false — the
// shape the schema already had, which is why confirm-before-enable needs no
// new state (WP-1.12-decisions.md §1). The login path ignores a secret whose
// TOTPEnabled is false, so starting an enrollment changes nothing about how
// the account authenticates until it is confirmed.
type User struct {
	ID              UserID
	TenantID        tenancy.ID
	Email           string
	PasswordHash    string
	TOTPSecret      string
	TOTPEnabled     bool
	TOTPLastCounter *int64
	CreatedAt       time.Time

	// Second-factor lockout (decisions §6). Only ever advanced after the
	// password has verified, so it is not a lever for an anonymous attacker.
	TOTPFailedAttempts int
	TOTPLockedUntil    *time.Time
}

// userColumns is the SELECT list every user lookup shares, so adding a column
// cannot be half-applied across the two lookups.
const userColumns = `id, tenant_id, email, password_hash, totp_secret, totp_enabled,
	totp_last_counter, created_at, totp_failed_attempts, totp_locked_until`

// CreateUser inserts a new user with a bcrypt password hash. tenant and
// email must be non-empty — INV-T2 requires every write to have an
// authorization-relevant scope, and an empty tenant would be a
// cross-tenant write by construction.
func CreateUser(ctx context.Context, db *storage.DB, tenant tenancy.ID, email, passwordHash string) (*User, error) {
	if tenant == "" || email == "" {
		return nil, errors.New("identity: tenant and email are required")
	}
	u := &User{
		ID:           UserID(idgen.New()),
		TenantID:     tenant,
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO users (id, tenant_id, email, password_hash, totp_enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`),
			string(u.ID), string(u.TenantID), u.Email, u.PasswordHash, false, u.CreatedAt)
		return err
	})
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
	var u *User
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT `+userColumns+`
			FROM users WHERE tenant_id = ? AND email = ?`), string(tenant), email)
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

// GetUserByID looks up a user scoped to tenant.
func GetUserByID(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) (*User, error) {
	var u *User
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT `+userColumns+`
			FROM users WHERE tenant_id = ? AND id = ?`), string(tenant), string(id))
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

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var idStr, tenantStr string
	var totpSecret sql.NullString
	var totpLastCounter sql.NullInt64
	var createdAt storage.Time
	var lockedUntil storage.NullTime
	err := row.Scan(&idStr, &tenantStr, &u.Email, &u.PasswordHash, &totpSecret, &u.TOTPEnabled,
		&totpLastCounter, &createdAt, &u.TOTPFailedAttempts, &lockedUntil)
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
	if lockedUntil.Valid {
		t := lockedUntil.Time.UTC()
		u.TOTPLockedUntil = &t
	}
	return &u, nil
}

// ErrTOTPAlreadyEnabled is returned when an enrollment is started on an account
// that already has a live second factor. Rotating one is disable-then-enroll,
// so that the audit trail says so (decisions §8).
var ErrTOTPAlreadyEnabled = errors.New("identity: TOTP is already enabled")

// ErrPasswordRequired is returned when TOTP enrollment is attempted on an
// account with no password. TOTP here is the second factor *of the password
// path*; on the OIDC path the IdP authenticates and MFA is the IdP's job
// (docs/08). Enrolling on a password-less account would produce an account that
// can neither use its second factor nor re-authenticate to remove it.
var ErrPasswordRequired = errors.New("identity: TOTP enrollment requires a password")

// StartTOTPEnrollment stores a freshly generated secret as *pending*: written
// to totp_secret with totp_enabled left false, which is what makes
// confirm-before-enable fall out of the existing schema (decisions §1). It
// returns the secret so the caller can render the otpauth:// URI and QR.
//
// Starting a second enrollment overwrites the pending secret — which is the
// mitigation for a half-finished enrollment, and the reason there is no
// pending-enrollment TTL.
func StartTOTPEnrollment(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) (string, error) {
	u, err := GetUserByID(ctx, db, tenant, id)
	if err != nil {
		return "", err
	}
	if u.TOTPEnabled {
		return "", ErrTOTPAlreadyEnabled
	}
	if u.PasswordHash == "" {
		return "", ErrPasswordRequired
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return "", err
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE tenant_id = ? AND id = ?`),
			secret, false, string(tenant), string(id))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("identity: start TOTP enrollment: %w", err)
	}
	return secret, nil
}

// ConfirmTOTPEnrollment validates code against the pending secret and, on a
// match, enables TOTP and mints the recovery codes — returning them, since this
// is the only moment they are ever readable.
//
// Enabling and minting share one transaction. Split across two, a failure
// between them leaves an account with a live second factor and no recovery
// codes the user ever saw, recoverable only by disable-then-re-enroll — and the
// obvious retry gets ErrTOTPAlreadyEnabled, so the one thing the user would try
// is the one thing that does not work.
//
// The matched counter is persisted in the same statement that flips the flag.
// Without that, totp_last_counter would still be NULL after enrollment and the
// very code used to confirm would remain usable as a login code for the rest of
// its 30-second step.
func ConfirmTOTPEnrollment(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, code string, at time.Time) ([]string, error) {
	u, err := GetUserByID(ctx, db, tenant, id)
	if err != nil {
		return nil, err
	}
	if u.TOTPEnabled {
		return nil, ErrTOTPAlreadyEnabled
	}
	if u.TOTPSecret == "" {
		return nil, ErrInvalidCredentials // nothing pending to confirm
	}
	ok, counter, err := ValidateTOTP(u.TOTPSecret, code, at, u.TOTPLastCounter)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}

	codes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET totp_enabled = ?, totp_last_counter = ?
			WHERE tenant_id = ? AND id = ?`),
			true, counter, string(tenant), string(id)); err != nil {
			return err
		}
		return replaceRecoveryCodes(ctx, tx, db, tenant, id, codes, at)
	})
	if err != nil {
		return nil, fmt.Errorf("identity: confirm TOTP enrollment: %w", err)
	}
	return codes, nil
}

// DisableTOTP clears the secret, the flag, the replay counter and the lockout
// state, and deletes every recovery code — a re-enrollment must not inherit
// codes minted against the previous secret.
//
// It does not re-authenticate: that is the caller's job, because the caller is
// the one holding the request (see PasswordTOTPProvider.Reauthenticate).
func DisableTOTP(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) error {
	if tenant == "" || id == "" {
		return errors.New("identity: tenant and user id are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, db.Rebind(`
			DELETE FROM totp_recovery_codes WHERE tenant_id = ? AND user_id = ?`),
			string(tenant), string(id)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET totp_secret = NULL, totp_enabled = ?, totp_last_counter = NULL,
				totp_failed_attempts = 0, totp_locked_until = NULL
			WHERE tenant_id = ? AND id = ?`),
			false, string(tenant), string(id))
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: disable TOTP: %w", err)
	}
	return nil
}

// Second-factor lockout policy (decisions §6). Ten attempts tolerates typos
// and a badly synced phone clock; fifteen minutes is short enough not to need
// an unlock path and long enough to turn a one-hour expected search of the
// live TOTP window into centuries.
const (
	totpMaxFailedAttempts = 10
	totpLockoutWindow     = 15 * time.Minute
)

// TOTPLockedAt reports whether the user's second factor is locked at t.
func (u *User) TOTPLockedAt(t time.Time) bool {
	return u.TOTPLockedUntil != nil && t.Before(*u.TOTPLockedUntil)
}

// recordTOTPFailure increments the consecutive-failure counter and, on the
// threshold, sets the lock.
//
// Both happen in SQL, against the stored value — never against the snapshot the
// caller read. Computing `attempts = u.TOTPFailedAttempts + 1` in Go and writing
// it back is a lost update: concurrent attempts all read the same k and all
// write k+1, so the counter never reaches the threshold, and worse, any request
// holding a pre-lock snapshot writes the lock back to NULL and *unlocks* the
// account. Both failures favour precisely the attacker this control exists to
// stop — one who already has the password and is running requests in parallel
// inside the gateway's budget. Same check-then-act trap ConsumeRecoveryCode
// avoids, one control over.
func recordTOTPFailure(ctx context.Context, db *storage.DB, tenant tenancy.ID, u *User, at time.Time) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Column references on the right-hand side read the pre-update row on
		// both dialects, so `totp_failed_attempts + 1` in the CASE is the value
		// being written. ELSE keeps any existing lock rather than clearing it.
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET
				totp_failed_attempts = totp_failed_attempts + 1,
				totp_locked_until = CASE
					WHEN totp_failed_attempts + 1 >= ? THEN ?
					ELSE totp_locked_until
				END
			WHERE tenant_id = ? AND id = ?`),
			totpMaxFailedAttempts, at.Add(totpLockoutWindow), string(tenant), string(u.ID))
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: record TOTP failure: %w", err)
	}
	return nil
}

// clearTOTPFailures resets the counter and lock after any successful second
// factor, so the threshold counts *consecutive* failures.
func clearTOTPFailures(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET totp_failed_attempts = 0, totp_locked_until = NULL
			WHERE tenant_id = ? AND id = ?`),
			string(tenant), string(id))
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: clear TOTP failures: %w", err)
	}
	return nil
}

// SetTOTPLastCounter persists the last-consumed TOTP step, closing the
// replay window (see ValidateTOTP).
func SetTOTPLastCounter(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, counter int64) error {
	if tenant == "" || id == "" {
		return errors.New("identity: tenant and user id are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE users SET totp_last_counter = ? WHERE tenant_id = ? AND id = ?`),
			counter, string(tenant), string(id))
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: set TOTP last counter: %w", err)
	}
	return nil
}

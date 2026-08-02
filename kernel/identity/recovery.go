// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Recovery codes are the way back in when the authenticator is gone. Ten are
// issued per enrollment; each is 15 bytes of crypto/rand rendered as 24
// characters of unpadded base32 in six dash-separated groups of four.
//
// 15 bytes rather than 16 because 120 bits divides evenly into base32
// characters and 128 does not. The base32 alphabet (A–Z, 2–7) has no 0/1/8/9
// to confuse with O/I/B/g.
const (
	RecoveryCodeCount = 10
	recoveryCodeBytes = 15
	recoveryGroupSize = 4
)

var recoveryEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateRecoveryCodes mints a fresh set for user, replacing any existing
// codes, and returns the plaintext codes. They are shown once and never
// recoverable afterwards: only the hashes are stored.
//
// ConfirmTOTPEnrollment does not call this — it shares replaceRecoveryCodes so
// that enabling the factor and minting its codes land in one transaction.
func GenerateRecoveryCodes(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) ([]string, error) {
	if tenant == "" || id == "" {
		return nil, errors.New("identity: tenant and user id are required")
	}
	codes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return replaceRecoveryCodes(ctx, tx, db, tenant, id, codes, now)
	})
	if err != nil {
		return nil, fmt.Errorf("identity: generate recovery codes: %w", err)
	}
	return codes, nil
}

// newRecoveryCodes returns one full set of plaintext codes.
func newRecoveryCodes() ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// replaceRecoveryCodes stores the hashes of codes, discarding whatever set was
// there. Replacing rather than adding: a new set invalidates the old one, so a
// user who regenerates because they think a code leaked is right to.
//
// It takes the transaction so callers can make the write atomic with whatever
// else has to happen alongside it.
func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, id UserID, codes []string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, db.Rebind(`
		DELETE FROM totp_recovery_codes WHERE tenant_id = ? AND user_id = ?`),
		string(tenant), string(id)); err != nil {
		return err
	}
	for _, code := range codes {
		if _, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO totp_recovery_codes (tenant_id, user_id, code_hash, created_at)
			VALUES (?, ?, ?, ?)`),
			string(tenant), string(id), hashRecoveryCode(code), at); err != nil {
			return err
		}
	}
	return nil
}

// ConsumeRecoveryCode marks one unused code as used and reports whether it did.
//
// Single-use is enforced by the database, not the application: the UPDATE
// carries `used_at IS NULL` and the caller requires exactly one affected row.
// Two concurrent logins presenting the same code therefore cannot both succeed
// on either dialect — the loser sees zero rows affected. Checking used_at in Go
// and then updating would be a check-then-act race; this is not.
func ConsumeRecoveryCode(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID, code string) (bool, error) {
	if tenant == "" || id == "" {
		return false, errors.New("identity: tenant and user id are required")
	}
	// An empty candidate would otherwise hash to the digest of "" and match
	// nothing — but only by luck; refuse it explicitly.
	if normalizeRecoveryCode(code) == "" {
		return false, nil
	}
	var consumed bool
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE totp_recovery_codes SET used_at = ?
			WHERE tenant_id = ? AND user_id = ? AND code_hash = ? AND used_at IS NULL`),
			time.Now().UTC(), string(tenant), string(id), hashRecoveryCode(code))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		consumed = n == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("identity: consume recovery code: %w", err)
	}
	return consumed, nil
}

// CountUnusedRecoveryCodes reports how many codes remain, for the account
// screen's "3 of 10 remaining".
func CountUnusedRecoveryCodes(ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) (int, error) {
	var n int
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(`
			SELECT COUNT(*) FROM totp_recovery_codes
			WHERE tenant_id = ? AND user_id = ? AND used_at IS NULL`),
			string(tenant), string(id)).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("identity: count recovery codes: %w", err)
	}
	return n, nil
}

// newRecoveryCode returns one formatted plaintext code.
func newRecoveryCode() (string, error) {
	b := make([]byte, recoveryCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: generate recovery code: %w", err)
	}
	raw := recoveryEnc.EncodeToString(b)

	var sb strings.Builder
	for i := 0; i < len(raw); i += recoveryGroupSize {
		if i > 0 {
			sb.WriteByte('-')
		}
		sb.WriteString(raw[i : i+recoveryGroupSize])
	}
	return sb.String(), nil
}

// normalizeRecoveryCode strips the formatting a user might reasonably type —
// dashes, spaces, lowercase — so the stored hash is of one canonical form.
func normalizeRecoveryCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		if (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// hashRecoveryCode is SHA-256 hex, unsalted — the same treatment session.go's
// hashToken gives a bearer token, for the same reason. A recovery code is 120
// bits from crypto/rand, so there is no precomputation to defeat and no
// low-entropy guess to make expensive; bcrypt's per-row salt would instead
// force one bcrypt per stored code on every login attempt that presents one,
// on an unauthenticated route (decisions §2).
//
// It normalizes first rather than trusting callers to: minting hashed the
// dashed display form while verification hashed the stripped one, so no code
// ever matched. One function owning the canonical form is what stops that
// recurring.
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

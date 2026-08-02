// SPDX-License-Identifier: AGPL-3.0-only

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

// Session audit actions. These are the values AuditSession writes to
// audit_log.action for authentication events.
const (
	AuditLogin  = "login"
	AuditLogout = "logout"
)

// Second-factor lifecycle audit actions (WP-1.12). Every transition is
// recorded — INV-T4 wants an attributable record of every mutation, and
// "someone turned MFA off on this account" is the one an incident review reads
// first. AuditRecoveryUsed additionally distinguishes a login that spent a
// recovery code from an ordinary one, which is why the login route offers
// recovery codes in their own field rather than sniffing them out of the TOTP
// field (decisions §5).
const (
	AuditTOTPEnrollStarted = "totp.enroll.started"
	AuditTOTPEnabled       = "totp.enabled"
	AuditTOTPDisabled      = "totp.disabled"
	AuditRecoveryUsed      = "totp.recovery.used"
)

// auditUserObject is the audit_log.object value for account-security events.
// They are recorded against the user, not a session: a TOTP transition outlives
// the session that made it, and an incident review asks "what happened to this
// account", not "what happened in this session".
const auditUserObject = "User"

// AuditAccountSecurity records a second-factor lifecycle transition against the
// user it happened to (INV-T4). Like AuditSession it lives in the kernel rather
// than the HTTP handler, so the record is a function call away rather than
// something each new caller has to remember.
func AuditAccountSecurity(ctx context.Context, db *storage.DB, tenant tenancy.ID, user UserID, action string) error {
	if tenant == "" || user == "" {
		return errors.New("identity: tenant and user are required to audit an account security event")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// changes stays "{}": the interesting fact is the transition itself,
		// and the columns it touches are a secret and its hashes. Writing them
		// here would defeat the point of hashing them over there.
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			idgen.New(), string(tenant), auditUserObject, string(user), action, "{}", string(user), time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: audit %s: %w", action, err)
	}
	return nil
}

// auditObject is the audit_log.object value for authentication events. Session
// is not a metadata object (sessions predate the metadata engine and are
// kernel-owned), but the audit trail is one table by design — docs/19 wants a
// single attributable record of every mutation, not one log per subsystem.
const auditObject = "Session"

// AuditSession records an authentication event against the actor it belongs to
// (INV-T4: every mutation is attributable — actor, action, timestamp). It lives
// here rather than in the HTTP handler so a second AuthProvider (OIDC, WP-1.9)
// gets the audit trail by calling one function instead of remembering to.
//
// The write runs under tenant context so RLS accepts it; sessions themselves
// are RLS-exempt (the token is what determines the tenant) but audit_log is
// not.
func AuditSession(ctx context.Context, db *storage.DB, tenant tenancy.ID, user UserID, session SessionID, action string) error {
	if tenant == "" || user == "" {
		return errors.New("identity: tenant and user are required to audit a session event")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			idgen.New(), string(tenant), auditObject, string(session), action, "{}", string(user), time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: audit session %s: %w", action, err)
	}
	return nil
}

// SessionOwner resolves a session id to the tenant and user it belongs to, so a
// logout can be audited against its principal after the token has been revoked.
func SessionOwner(ctx context.Context, db *storage.DB, id SessionID) (tenancy.ID, UserID, error) {
	var tenantStr, userStr string
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT tenant_id, user_id FROM sessions WHERE id = ?`), string(id))
	if err := row.Scan(&tenantStr, &userStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrSessionInvalid
		}
		return "", "", fmt.Errorf("identity: session owner: %w", err)
	}
	return tenancy.ID(tenantStr), UserID(userStr), nil
}

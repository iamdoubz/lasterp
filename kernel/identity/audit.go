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

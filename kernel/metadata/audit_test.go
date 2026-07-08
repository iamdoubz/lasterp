package metadata

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The audit trail is the mechanism that discharges INV-T4 for CRUD-domain
// writes (docs/notes/WP-0.5-decisions.md, decision 6) — the database
// itself rejects UPDATE/DELETE on audit_log, not just the application.
func TestAuditLogIsAppendOnly(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			var auditID string
			err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
				auditID = idgen.New()
				_, err := tx.ExecContext(ctx, db.Rebind(`
					INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
					auditID, string(tenant), "Contact", "rec-1", "create", `{}`, "user-1", time.Now().UTC())
				return err
			})
			if err != nil {
				t.Fatalf("insert audit row: %v", err)
			}

			mutate := func(query string, args ...any) error {
				return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, db.Rebind(query), args...)
					return err
				})
			}
			if err := mutate(`UPDATE audit_log SET action = ? WHERE id = ?`, "tampered", auditID); err == nil {
				t.Fatal("UPDATE on audit_log succeeded, want rejection by the append-only trigger")
			}
			if err := mutate(`DELETE FROM audit_log WHERE id = ?`, auditID); err == nil {
				t.Fatal("DELETE on audit_log succeeded, want rejection by the append-only trigger")
			}
		})
	}
}

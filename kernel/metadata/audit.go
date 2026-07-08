// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// recordAudit writes one audit_log row inside tx — the same transaction
// as the data write it documents, so the two are atomic. INV-T4: every
// mutation is attributable (decision 6) — actorID must be non-empty.
func recordAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, object, recordID, action string, changes json.RawMessage, actorID string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), object, recordID, action, string(changes), actorID, time.Now().UTC())
	return err
}

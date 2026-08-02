// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// recordAudit writes one audit_log row inside tx — the same transaction
// as the data write it documents, so the two are atomic. INV-T4: every
// mutation is attributable (decision 6) — actorID must be non-empty.
func recordAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, object, recordID, action string, changes json.RawMessage, actorID string) error {
	id, at := idgen.New(), time.Now().UTC()
	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		id, string(tenant), object, recordID, action, string(changes), actorID, at)
	if err != nil {
		return err
	}

	// Master-data changes are half the feed a replica follows — docs/04 counts
	// "CRUD audit entries" alongside event-store entries — so publish in this
	// same transaction, for the same reason the audit row itself is written
	// here: the two must be atomic with the write they document (WP-2.1).
	return changefeed.Append(ctx, tx, db, changefeed.Entry{
		TenantID:   tenant,
		Source:     changefeed.SourceAudit,
		RefID:      id,
		Object:     object,
		ScopeKey:   changefeed.ScopeKeyFor(object),
		RecordedAt: at,
	})
}

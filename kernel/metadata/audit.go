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
	//
	// RefID is the **record's** id, not this audit row's. WP-2.1 wrote the
	// audit id here, matching its own "source + ref_id identify the row in
	// events or audit_log" — which had no consumer yet to be wrong for. The
	// first one (WP-2.2a materialisation) needs the row that changed, and an
	// audit id only reaches it by a second hop. The audit row itself stays
	// reachable by (tenant_id, object, record_id), which is exactly the index
	// audit_log already carries.
	return changefeed.Append(ctx, tx, db, changefeed.Entry{
		TenantID: tenant,
		Source:   changefeed.SourceAudit,
		RefID:    recordID,
		Object:   object,
		ScopeKey: changefeed.ScopeKeyFor(object),
		// The actor was already being written to audit_log and dropped on the
		// way to the feed. WP-3.1b needs it: an async hook runner skips changes
		// the plugin itself made, and without an author on the entry a plugin
		// reacts to its own writes forever.
		ActorID:    actorID,
		RecordedAt: at,
	})
}

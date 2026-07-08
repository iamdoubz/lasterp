// SPDX-License-Identifier: AGPL-3.0-only

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Snapshot is a point-in-time fold of a stream, letting LoadStream skip
// replaying from event zero. One per stream: SaveSnapshot overwrites
// whatever was there before.
type Snapshot struct {
	TenantID   tenancy.ID
	StreamID   StreamID
	Version    int
	State      json.RawMessage
	RecordedAt time.Time
}

// SaveSnapshot records state as of version. It does not decide *when* to
// snapshot — that's a per-aggregate policy call for whichever module owns
// the stream (docs/notes/WP-0.4-decisions.md, decision 2).
func SaveSnapshot(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream StreamID, version int, state json.RawMessage) error {
	if tenant == "" || stream == "" {
		return errors.New("eventstore: tenant and stream are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO stream_snapshots (tenant_id, stream_id, version, state, recorded_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, stream_id) DO UPDATE SET
				version = excluded.version, state = excluded.state, recorded_at = excluded.recorded_at`),
			string(tenant), string(stream), version, string(state), time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("eventstore: save snapshot: %w", err)
	}
	return nil
}

// LoadSnapshot returns stream's snapshot, or nil if none has been taken.
func LoadSnapshot(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream StreamID) (*Snapshot, error) {
	if tenant == "" || stream == "" {
		return nil, errors.New("eventstore: tenant and stream are required")
	}
	var s *Snapshot
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT tenant_id, stream_id, version, state, recorded_at
			FROM stream_snapshots WHERE tenant_id = ? AND stream_id = ?`), string(tenant), string(stream))

		var got Snapshot
		var tenantStr, streamStr, state string
		var recordedAt storage.Time
		scanErr := row.Scan(&tenantStr, &streamStr, &got.Version, &state, &recordedAt)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		got.TenantID = tenancy.ID(tenantStr)
		got.StreamID = StreamID(streamStr)
		got.State = json.RawMessage(state)
		got.RecordedAt = recordedAt.Time
		s = &got
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("eventstore: load snapshot: %w", err)
	}
	return s, nil
}

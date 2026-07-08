// SPDX-License-Identifier: AGPL-3.0-only

package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ReadFeed returns up to limit events for tenant with id > afterCursor,
// ordered by id ascending — the cursor-based read primitive over the
// global change feed position (docs/03, docs/04). It does not push;
// callers page by passing back the last event's ID as the next
// afterCursor. INV-E5: replaying the same (tenant, afterCursor, limit)
// sequence against unchanged data always returns the same result, since
// it is a pure function of committed rows.
func ReadFeed(ctx context.Context, db *storage.DB, tenant tenancy.ID, afterCursor int64, limit int) ([]Event, error) {
	if tenant == "" {
		return nil, errors.New("eventstore: tenant is required")
	}
	if limit <= 0 {
		return nil, errors.New("eventstore: limit must be positive")
	}

	var events []Event
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at
			FROM events WHERE tenant_id = ? AND id > ? ORDER BY id ASC LIMIT ?`),
			string(tenant), afterCursor, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("eventstore: read feed: %w", err)
	}
	return events, nil
}

// LoadStream folds stream from its latest snapshot (if any) plus every
// event after the snapshot's version, applying upcasters (nil is fine —
// no-op) to each event's payload. It does not itself compute a projection;
// it returns the raw (upcasted) event sequence for the caller to fold.
func LoadStream(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream StreamID, upcasters *Upcasters) (*Snapshot, []Event, error) {
	if tenant == "" || stream == "" {
		return nil, nil, errors.New("eventstore: tenant and stream are required")
	}
	snapshot, err := LoadSnapshot(ctx, db, tenant, stream)
	if err != nil {
		return nil, nil, err
	}
	afterVersion := 0
	if snapshot != nil {
		afterVersion = snapshot.Version
	}

	var events []Event
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at
			FROM events WHERE tenant_id = ? AND stream_id = ? AND version > ? ORDER BY version ASC`),
			string(tenant), string(stream), afterVersion)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			e, err = upcasters.Apply(e)
			if err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("eventstore: load stream: %w", err)
	}
	return snapshot, events, nil
}

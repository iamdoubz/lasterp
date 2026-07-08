// SPDX-License-Identifier: AGPL-3.0-only

// Package eventstore is the WP-0.4 kernel: an append-only event log with
// optimistic concurrency (ADR-003). Every query takes tenant explicitly
// and filters on it — defense in depth alongside Postgres RLS (INV-T1),
// and the only guard at all on SQLite, where RLS doesn't apply (ADR-005
// solo-mode bypass).
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

// StreamID identifies one aggregate's event stream within a tenant.
type StreamID string

// EventType names an event's shape, e.g. "invoice.posted".
type EventType string

// NewEvent is the input to Append: everything the caller supplies about a
// single event, before the store assigns it a version and recorded_at.
type NewEvent struct {
	Type          EventType
	SchemaVersion int
	Payload       json.RawMessage
	ActorID       string    // INV-T4: never empty — no anonymous writes.
	OccurredAt    time.Time // caller-reported; kept for forensics only (docs/04).
}

// Event is a committed row: NewEvent plus everything the store assigned.
type Event struct {
	ID            int64 // global cursor position (docs/03: change feed reads this)
	TenantID      tenancy.ID
	StreamID      StreamID
	Version       int
	Type          EventType
	SchemaVersion int
	Payload       json.RawMessage
	ActorID       string
	CommandID     string
	OccurredAt    time.Time
	RecordedAt    time.Time // server-assigned; authoritative for ordering (docs/04).
}

// ErrVersionConflict is returned by Append when afterVersion no longer
// matches the stream's current version (INV-E2): another writer committed
// first. The caller is expected to reload CurrentVersion and retry
// (ADR-003: "handler reloads, revalidates, retries").
var ErrVersionConflict = errors.New("eventstore: stream version conflict")

// CurrentVersion returns stream's latest committed version, or 0 if the
// stream has no events yet.
func CurrentVersion(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream StreamID) (int, error) {
	if tenant == "" || stream == "" {
		return 0, errors.New("eventstore: tenant and stream are required")
	}
	var version int
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var v sql.NullInt64
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT MAX(version) FROM events WHERE tenant_id = ? AND stream_id = ?`),
			string(tenant), string(stream))
		if err := row.Scan(&v); err != nil {
			return err
		}
		version = int(v.Int64)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("eventstore: current version: %w", err)
	}
	return version, nil
}

// Append commits ev to stream as version afterVersion+1, scoped to tenant.
//
// If commandID was already committed (anywhere, since command_id is
// globally unique — docs/03), Append does not re-append: it returns the
// event already recorded for that command (INV-E4, exactly-once effect on
// retry) rather than erroring or double-applying. Otherwise, if
// afterVersion no longer matches the stream's current version, Append
// returns ErrVersionConflict (INV-E2) without committing anything.
func Append(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream StreamID, afterVersion int, commandID string, ev NewEvent) (Event, error) {
	if tenant == "" || stream == "" {
		return Event{}, errors.New("eventstore: tenant and stream are required")
	}
	if commandID == "" {
		return Event{}, errors.New("eventstore: commandID is required")
	}
	if ev.ActorID == "" {
		return Event{}, errors.New("eventstore: actor is required")
	}
	if ev.Type == "" {
		return Event{}, errors.New("eventstore: event type is required")
	}

	recordedAt := time.Now().UTC()
	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = recordedAt
	}
	version := afterVersion + 1

	var result Event
	insertErr := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Check command_id first, inside the same transaction as the
		// insert attempt: a retried command naturally re-targets the same
		// version the original attempt did, so a replay can trip *both*
		// unique indexes (command_id and tenant+stream+version)
		// simultaneously — which one Postgres reports is not guaranteed.
		// Checking first removes the ambiguity: if commandID is already
		// committed, that's what matters, regardless of what the blind
		// insert's error would have said.
		if existing, found, err := scanByCommandID(ctx, tx, db, tenant, commandID); err != nil {
			return err
		} else if found {
			result = existing
			return nil
		}

		row := tx.QueryRowContext(ctx, db.Rebind(`
			INSERT INTO events (tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`),
			string(tenant), string(stream), version, string(ev.Type), ev.SchemaVersion,
			string(ev.Payload), ev.ActorID, commandID, occurredAt, recordedAt)

		var id int64
		if err := row.Scan(&id); err != nil {
			return err
		}
		result = Event{
			ID: id, TenantID: tenant, StreamID: stream, Version: version,
			Type: ev.Type, SchemaVersion: ev.SchemaVersion, Payload: ev.Payload,
			ActorID: ev.ActorID, CommandID: commandID,
			OccurredAt: occurredAt, RecordedAt: recordedAt,
		}
		return nil
	})

	switch {
	case insertErr == nil:
		return result, nil
	case storage.IsUniqueViolation(insertErr):
		// The command_id check above already ruled out a replay in the
		// common case; this remaining path is the rare genuine race (two
		// truly concurrent attempts with the same commandID) — the
		// failed INSERT's transaction is already rolled back (see
		// tenancy.WithTenant), so a fresh transaction reads whichever
		// racer actually won.
		if existing, found, err := lookupByCommandID(ctx, db, tenant, commandID); err != nil {
			return Event{}, err
		} else if found {
			return existing, nil
		}
		return Event{}, ErrVersionConflict
	default:
		return Event{}, fmt.Errorf("eventstore: append: %w", insertErr)
	}
}

func scanByCommandID(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, commandID string) (Event, bool, error) {
	row := tx.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at
		FROM events WHERE tenant_id = ? AND command_id = ?`), string(tenant), commandID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return e, true, nil
}

func lookupByCommandID(ctx context.Context, db *storage.DB, tenant tenancy.ID, commandID string) (Event, bool, error) {
	var e Event
	var found bool
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		got, ok, scanErr := scanByCommandID(ctx, tx, db, tenant, commandID)
		if scanErr != nil {
			return scanErr
		}
		e, found = got, ok
		return nil
	})
	if err != nil {
		return Event{}, false, err
	}
	return e, found, nil
}

// scanner is the common subset of *sql.Row and *sql.Rows scanEvent needs,
// so a single-row lookup and a feed/stream query share one scan body.
type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (Event, error) {
	var e Event
	var tenantStr, streamStr, typeStr, payload string
	var occurredAt, recordedAt storage.Time
	err := row.Scan(&e.ID, &tenantStr, &streamStr, &e.Version, &typeStr, &e.SchemaVersion,
		&payload, &e.ActorID, &e.CommandID, &occurredAt, &recordedAt)
	if err != nil {
		return Event{}, fmt.Errorf("eventstore: scan event: %w", err)
	}
	e.TenantID = tenancy.ID(tenantStr)
	e.StreamID = StreamID(streamStr)
	e.Type = EventType(typeStr)
	e.Payload = json.RawMessage(payload)
	e.OccurredAt = occurredAt.Time
	e.RecordedAt = recordedAt.Time
	return e, nil
}

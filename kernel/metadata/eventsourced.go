// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// EventSourced is the write-side engine for a persistence: event_sourced object
// (ADR-003) — the event-sourced counterpart to CRUD, and the metadata-engine
// support for event-sourced objects deferred from WP-0.5 (decision 2). It is
// the choke point that makes every event-sourced write authorized (INV-T2) and
// attributable (INV-T4): Emit authorizes the action against the object's
// declared permissions, then appends through kernel/eventstore, whose
// append-only events table is itself the audit trail (ADR-003) — so unlike a
// CRUD write there is no separate audit_log row. Domain rules (a journal entry
// must balance; its period must be open) live in the owning module and run
// before Emit.
type EventSourced struct {
	schema *EffectiveSchema
}

// ErrNotEventSourced is returned by NewEventSourced for a non-event-sourced
// object.
var ErrNotEventSourced = errors.New(`metadata: EventSourced engine requires persistence "event_sourced"`)

// NewEventSourced builds the engine for an event-sourced object.
func NewEventSourced(schema *EffectiveSchema) (*EventSourced, error) {
	if schema.Persistence != PersistenceEventSourced {
		return nil, fmt.Errorf("%w (got %q)", ErrNotEventSourced, schema.Persistence)
	}
	return &EventSourced{schema: schema}, nil
}

// Object returns the object's name.
func (e *EventSourced) Object() string { return e.schema.ObjectName }

// Emit authorizes action against the object's permissions (INV-T2), stamps the
// authorizing actor onto the event (INV-T4 — never the caller's word for who
// they are), and appends ev to stream at afterVersion (INV-E2 optimistic
// concurrency; commandID gives INV-E4 idempotency). Returns the committed
// event.
func (e *EventSourced) Emit(ctx context.Context, db *storage.DB, tenant tenancy.ID, action string, stream eventstore.StreamID, afterVersion int, commandID string, ev eventstore.NewEvent) (eventstore.Event, error) {
	actor, err := authz.Authorize(ctx, db, e.schema.ObjectName, action)
	if err != nil {
		return eventstore.Event{}, err
	}
	ev.ActorID = string(actor.UserID)
	return eventstore.Append(ctx, db, tenant, stream, afterVersion, commandID, ev)
}

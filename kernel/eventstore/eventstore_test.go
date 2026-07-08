package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

func TestAppendAndCurrentVersion(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			if v, err := CurrentVersion(ctx, db, tenant, stream); err != nil || v != 0 {
				t.Fatalf("CurrentVersion on empty stream = (%d, %v), want (0, nil)", v, err)
			}

			ev, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1,
				Payload: json.RawMessage(`{"total":100}`), ActorID: "user-1",
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if ev.Version != 1 {
				t.Fatalf("Version = %d, want 1", ev.Version)
			}

			if v, err := CurrentVersion(ctx, db, tenant, stream); err != nil || v != 1 {
				t.Fatalf("CurrentVersion after append = (%d, %v), want (1, nil)", v, err)
			}
		})
	}
}

// INV-E2: a stale afterVersion is rejected, never silently merged.
func TestAppendRejectsStaleVersion(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			if _, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1",
			}); err != nil {
				t.Fatalf("first Append: %v", err)
			}

			_, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1",
			})
			if !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("second Append at stale version: err = %v, want ErrVersionConflict", err)
			}
		})
	}
}

// INV-E4: replaying the same command_id doesn't double-append — it
// returns the event already committed for that command.
func TestAppendIdempotentOnDuplicateCommandID(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())
			commandID := idgen.New()

			first, err := Append(ctx, db, tenant, stream, 0, commandID, NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{"total":1}`), ActorID: "user-1",
			})
			if err != nil {
				t.Fatalf("first Append: %v", err)
			}

			second, err := Append(ctx, db, tenant, stream, 0, commandID, NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{"total":1}`), ActorID: "user-1",
			})
			if err != nil {
				t.Fatalf("replayed Append: %v", err)
			}
			if second.ID != first.ID || second.Version != first.Version {
				t.Fatalf("replayed Append returned a different event: got %+v, want %+v", second, first)
			}

			if v, err := CurrentVersion(ctx, db, tenant, stream); err != nil || v != 1 {
				t.Fatalf("CurrentVersion after replay = (%d, %v), want (1, nil) — replay must not double-append", v, err)
			}
		})
	}
}

func TestAppendRequiresActorAndType(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			if _, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", Payload: json.RawMessage(`{}`),
			}); err == nil {
				t.Fatal("Append with empty ActorID succeeded, want error")
			}
			if _, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				ActorID: "user-1", Payload: json.RawMessage(`{}`),
			}); err == nil {
				t.Fatal("Append with empty Type succeeded, want error")
			}
		})
	}
}

//go:build integrity

package eventstore

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// INV-S5: every committed event reaches the change feed, in the same order and
// in the same transaction. A replica's view of the log is only as complete as
// this — an event the feed never mentions is an event no client will ever see.
func TestAppendPublishesToChangeFeed(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			var ids []int64
			for i := 1; i <= 5; i++ {
				ev, err := Append(ctx, db, tenant, stream, i-1, idgen.New(), NewEvent{
					Type: "invoice.line_added", SchemaVersion: 1,
					Payload: json.RawMessage(`{}`), ActorID: "user-1",
				})
				if err != nil {
					t.Fatalf("append %d: %v", i, err)
				}
				ids = append(ids, ev.ID)
			}

			changes, err := changefeed.Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read feed: %v", err)
			}
			if len(changes) != len(ids) {
				t.Fatalf("feed has %d entries for %d appended events", len(changes), len(ids))
			}
			for i, c := range changes {
				if c.Source != changefeed.SourceEvent {
					t.Fatalf("entry %d source = %q, want %q", i, c.Source, changefeed.SourceEvent)
				}
				if want := strconv.FormatInt(ids[i], 10); c.RefID != want {
					t.Fatalf("entry %d points at event %q, want %q (feed order must match append order)", i, c.RefID, want)
				}
				if c.Object != "invoice" {
					t.Fatalf("entry %d object = %q, want the stream's kind", i, c.Object)
				}
			}
		})
	}
}

// A replayed command is idempotent (INV-E4) and must not double-publish: the
// second Append returns the original event without appending, so the feed
// keeps exactly one entry for it. Two entries would make a replica apply the
// same change twice.
func TestReplayedCommandPublishesOnce(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())
			commandID := idgen.New()

			ev := NewEvent{Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1"}
			first, err := Append(ctx, db, tenant, stream, 0, commandID, ev)
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			second, err := Append(ctx, db, tenant, stream, 0, commandID, ev)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if first.ID != second.ID {
				t.Fatalf("replay produced a new event %d, want the original %d", second.ID, first.ID)
			}

			changes, err := changefeed.Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read feed: %v", err)
			}
			if len(changes) != 1 {
				t.Fatalf("feed has %d entries after a replayed command, want 1: %+v", len(changes), changes)
			}
		})
	}
}

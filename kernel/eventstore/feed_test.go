package eventstore

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// INV-E5 + the WP-0.4 AC (feed replay determinism): reading the feed from
// the same cursor twice returns identical results — it is a pure function
// of committed state, not something that drifts between reads.
func TestFeedReplayDeterminism(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			for i := 1; i <= 10; i++ {
				if _, err := Append(ctx, db, tenant, stream, i-1, idgen.New(), NewEvent{
					Type: "invoice.line_added", SchemaVersion: 1,
					Payload: json.RawMessage(`{}`), ActorID: "user-1",
				}); err != nil {
					t.Fatalf("Append %d: %v", i, err)
				}
			}

			first, err := ReadFeed(ctx, db, tenant, 0, 100)
			if err != nil {
				t.Fatalf("ReadFeed (first): %v", err)
			}
			if len(first) != 10 {
				t.Fatalf("got %d events, want 10", len(first))
			}
			for i := 1; i < len(first); i++ {
				if first[i].ID <= first[i-1].ID {
					t.Fatalf("feed not strictly increasing by id at index %d: %d <= %d", i, first[i].ID, first[i-1].ID)
				}
			}

			second, err := ReadFeed(ctx, db, tenant, 0, 100)
			if err != nil {
				t.Fatalf("ReadFeed (second): %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("replaying the same cursor returned a different result:\nfirst:  %+v\nsecond: %+v", first, second)
			}

			// Paging: reading in two pages from cursor 0 reconstructs the
			// same sequence as one unpaged read.
			page1, err := ReadFeed(ctx, db, tenant, 0, 4)
			if err != nil {
				t.Fatalf("ReadFeed (page1): %v", err)
			}
			page2, err := ReadFeed(ctx, db, tenant, page1[len(page1)-1].ID, 100)
			if err != nil {
				t.Fatalf("ReadFeed (page2): %v", err)
			}
			paged := append(page1, page2...)
			if !reflect.DeepEqual(first, paged) {
				t.Fatalf("paged read disagrees with unpaged read:\nunpaged: %+v\npaged:   %+v", first, paged)
			}
		})
	}
}

// The feed is per-tenant: reading tenant A's feed never surfaces tenant
// B's events (INV-T1, same as the WP-0.3 kernel/tenancy tests).
func TestFeedIsTenantScoped(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			if _, err := Append(ctx, db, tenantA, StreamID("invoice:"+idgen.New()), 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1",
			}); err != nil {
				t.Fatalf("Append (tenant A): %v", err)
			}
			if _, err := Append(ctx, db, tenantB, StreamID("invoice:"+idgen.New()), 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1",
			}); err != nil {
				t.Fatalf("Append (tenant B): %v", err)
			}

			feedA, err := ReadFeed(ctx, db, tenantA, 0, 100)
			if err != nil {
				t.Fatalf("ReadFeed (tenant A): %v", err)
			}
			if len(feedA) != 1 || feedA[0].TenantID != tenantA {
				t.Fatalf("tenant A's feed = %+v, want exactly 1 event belonging to tenant A", feedA)
			}
		})
	}
}

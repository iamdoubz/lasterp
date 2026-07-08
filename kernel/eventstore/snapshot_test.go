package eventstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

func TestSnapshotRoundTrip(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			if s, err := LoadSnapshot(ctx, db, tenant, stream); err != nil || s != nil {
				t.Fatalf("LoadSnapshot on stream with no snapshot = (%+v, %v), want (nil, nil)", s, err)
			}

			if err := SaveSnapshot(ctx, db, tenant, stream, 3, json.RawMessage(`{"total":300}`)); err != nil {
				t.Fatalf("SaveSnapshot: %v", err)
			}
			s, err := LoadSnapshot(ctx, db, tenant, stream)
			if err != nil {
				t.Fatalf("LoadSnapshot: %v", err)
			}
			if s == nil || s.Version != 3 || string(s.State) != `{"total":300}` {
				t.Fatalf("LoadSnapshot = %+v, want version 3 state {\"total\":300}", s)
			}

			// Overwrite: SaveSnapshot replaces, doesn't accumulate.
			if err := SaveSnapshot(ctx, db, tenant, stream, 5, json.RawMessage(`{"total":500}`)); err != nil {
				t.Fatalf("SaveSnapshot (overwrite): %v", err)
			}
			s, err = LoadSnapshot(ctx, db, tenant, stream)
			if err != nil {
				t.Fatalf("LoadSnapshot (after overwrite): %v", err)
			}
			if s.Version != 5 || string(s.State) != `{"total":500}` {
				t.Fatalf("LoadSnapshot after overwrite = %+v, want version 5 state {\"total\":500}", s)
			}
		})
	}
}

// INV-E5: LoadStream via a snapshot + tail agrees with a full replay from
// event zero — snapshotting is an optimization, not a different truth.
func TestLoadStreamViaSnapshotAgreesWithFullReplay(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			for i := 1; i <= 5; i++ {
				if _, err := Append(ctx, db, tenant, stream, i-1, idgen.New(), NewEvent{
					Type: "invoice.line_added", SchemaVersion: 1,
					Payload: json.RawMessage(`{}`), ActorID: "user-1",
				}); err != nil {
					t.Fatalf("Append %d: %v", i, err)
				}
			}

			_, fullReplay, err := LoadStream(ctx, db, tenant, stream, nil)
			if err != nil {
				t.Fatalf("LoadStream (full replay): %v", err)
			}
			if len(fullReplay) != 5 {
				t.Fatalf("full replay has %d events, want 5", len(fullReplay))
			}

			if err := SaveSnapshot(ctx, db, tenant, stream, 3, json.RawMessage(`{}`)); err != nil {
				t.Fatalf("SaveSnapshot: %v", err)
			}
			snapshot, tail, err := LoadStream(ctx, db, tenant, stream, nil)
			if err != nil {
				t.Fatalf("LoadStream (via snapshot): %v", err)
			}
			if snapshot == nil || snapshot.Version != 3 {
				t.Fatalf("snapshot = %+v, want version 3", snapshot)
			}
			if len(tail) != 2 {
				t.Fatalf("tail after snapshot has %d events, want 2 (versions 4,5)", len(tail))
			}
			if tail[0].Version != 4 || tail[1].Version != 5 {
				t.Fatalf("tail versions = [%d,%d], want [4,5]", tail[0].Version, tail[1].Version)
			}

			// Effective final version agrees either way.
			finalFromFullReplay := fullReplay[len(fullReplay)-1].Version
			finalFromSnapshotTail := tail[len(tail)-1].Version
			if finalFromFullReplay != finalFromSnapshotTail {
				t.Fatalf("final version disagrees: full replay %d vs snapshot+tail %d", finalFromFullReplay, finalFromSnapshotTail)
			}
		})
	}
}

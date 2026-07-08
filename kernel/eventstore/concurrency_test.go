package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// INV-E2 + the WP-0.4 AC: 1000 concurrent writers racing to append to one
// stream, each retrying on ErrVersionConflict until it succeeds — the
// stream must end up with exactly 1000 events, versions 1..1000
// contiguous, no gaps and no duplicates.
func TestConcurrencyTorture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1000-writer torture test in -short mode")
	}
	const writers = 1000

	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			var wg sync.WaitGroup
			errCh := make(chan error, writers)
			for i := range writers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					commandID := idgen.New()
					for {
						v, err := CurrentVersion(ctx, db, tenant, stream)
						if err != nil {
							errCh <- fmt.Errorf("writer %d: CurrentVersion: %w", i, err)
							return
						}
						_, err = Append(ctx, db, tenant, stream, v, commandID, NewEvent{
							Type: "counter.incremented", SchemaVersion: 1,
							Payload: json.RawMessage(fmt.Sprintf(`{"writer":%d}`, i)),
							ActorID: "torture-writer",
						})
						if errors.Is(err, ErrVersionConflict) {
							continue
						}
						if err != nil {
							errCh <- fmt.Errorf("writer %d: Append: %w", i, err)
						}
						return
					}
				}(i)
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Fatal(err)
			}

			_, events, err := LoadStream(ctx, db, tenant, stream, nil)
			if err != nil {
				t.Fatalf("LoadStream: %v", err)
			}
			if len(events) != writers {
				t.Fatalf("stream has %d events, want %d", len(events), writers)
			}

			seen := make(map[int]bool, writers)
			for _, e := range events {
				if seen[e.Version] {
					t.Fatalf("duplicate version %d in stream", e.Version)
				}
				seen[e.Version] = true
			}
			for v := 1; v <= writers; v++ {
				if !seen[v] {
					t.Fatalf("missing version %d — gap in stream", v)
				}
			}
		})
	}
}

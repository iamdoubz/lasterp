package eventstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// INV-E3: events are immutable post-commit; schema evolution happens via
// upcasters applied on read, never by rewriting the stored row.
func TestUpcasterAppliedOnReadNotStored(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			// v1 shape: {"amount": 100}. v2 shape adds a currency field.
			if _, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1,
				Payload: json.RawMessage(`{"amount":100}`), ActorID: "user-1",
			}); err != nil {
				t.Fatalf("Append: %v", err)
			}

			upcasters := NewUpcasters()
			upcasters.Register("invoice.created", 1, func(payload []byte) ([]byte, error) {
				var v1 struct {
					Amount int `json:"amount"`
				}
				if err := json.Unmarshal(payload, &v1); err != nil {
					return nil, err
				}
				return json.Marshal(map[string]any{"amount": v1.Amount, "currency": "USD"})
			})

			_, events, err := LoadStream(ctx, db, tenant, stream, upcasters)
			if err != nil {
				t.Fatalf("LoadStream: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			if events[0].SchemaVersion != 2 {
				t.Fatalf("SchemaVersion = %d, want 2 (upcasted)", events[0].SchemaVersion)
			}
			var upcasted map[string]any
			if err := json.Unmarshal(events[0].Payload, &upcasted); err != nil {
				t.Fatalf("unmarshal upcasted payload: %v", err)
			}
			if upcasted["currency"] != "USD" {
				t.Fatalf("upcasted payload = %v, want currency=USD", upcasted)
			}

			// The stored row itself must be untouched (INV-E3): reading
			// without an upcaster registry still returns the original v1 shape.
			_, rawEvents, err := LoadStream(ctx, db, tenant, stream, nil)
			if err != nil {
				t.Fatalf("LoadStream (no upcasters): %v", err)
			}
			if rawEvents[0].SchemaVersion != 1 {
				t.Fatalf("stored SchemaVersion = %d, want 1 (unchanged by upcasting)", rawEvents[0].SchemaVersion)
			}
			var original map[string]any
			if err := json.Unmarshal(rawEvents[0].Payload, &original); err != nil {
				t.Fatalf("unmarshal stored payload: %v", err)
			}
			if _, hasCurrency := original["currency"]; hasCurrency {
				t.Fatal("stored payload gained a currency field — upcasting must not rewrite the row")
			}
		})
	}
}

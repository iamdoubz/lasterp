package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// INV-E1 (partial — see docs/notes/WP-0.4-decisions.md, decision 1): the
// database itself rejects UPDATE/DELETE on events, not just the
// application layer.
func TestEventsAreAppendOnly(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			stream := StreamID("invoice:" + idgen.New())

			ev, err := Append(ctx, db, tenant, stream, 0, idgen.New(), NewEvent{
				Type: "invoice.created", SchemaVersion: 1, Payload: json.RawMessage(`{}`), ActorID: "user-1",
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}

			// Tenant context must be set for the row to be visible/matchable
			// under RLS at all — otherwise UPDATE/DELETE would "succeed"
			// having silently matched zero rows, without ever reaching the
			// trigger, which would prove nothing.
			mutate := func(query string, args ...any) error {
				return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, db.Rebind(query), args...)
					return err
				})
			}

			if err := mutate(`UPDATE events SET actor_id = ? WHERE id = ?`, "someone-else", ev.ID); err == nil {
				t.Fatal("UPDATE on events succeeded, want rejection by the append-only trigger")
			}
			if err := mutate(`DELETE FROM events WHERE id = ?`, ev.ID); err == nil {
				t.Fatal("DELETE on events succeeded, want rejection by the append-only trigger")
			}

			if v, err := CurrentVersion(ctx, db, tenant, stream); err != nil || v != 1 {
				t.Fatalf("CurrentVersion after rejected mutations = (%d, %v), want (1, nil)", v, err)
			}
		})
	}
}

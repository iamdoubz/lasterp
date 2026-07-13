//go:build integrity

package integrity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// isInsufficientPrivilege reports whether err is a Postgres 42501
// (insufficient_privilege) — the code the server returns when a role lacks a
// grant. Asserting on it (not just err != nil) is what isolates role
// separation from the append-only trigger: without the REVOKE the app role
// still holds the grant, so the statement would reach the trigger and fail
// with P0001 instead — a different code, and this assertion goes red.
func isInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// The adversarial writer suite (docs/19 §3): each test *attempts* a known
// integrity bypass and asserts it fails. New enforcement is proven by the
// attack failing; regress the enforcement and the attack succeeds, turning
// the test red. This is v1 — the Phase-0 write surface (events, audit_log,
// RLS). Module WPs extend it with their own attacks (unbalanced entries,
// closed-period posts, float money — no code to attack until Phase 1).

// mutate runs a statement inside the tenant's transaction so RLS makes the
// target row visible/checkable — otherwise an UPDATE/DELETE could "succeed"
// by matching zero rows and prove nothing.
func mutate(db *storage.DB, tenant tenancy.ID, query string, args ...any) error {
	return tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(query), args...)
		return err
	})
}

// INV-E1 / INV-E3 (events) and INV-T4 (audit_log): with DB role separation
// in place, the app role has no UPDATE/DELETE grant on the append-only
// tables — the mutation is impossible, not merely trigger-forbidden. Only
// meaningful on Postgres; SQLite has no roles (the trigger is its guard, see
// TestAdversarial_TriggerRejectsOwnerMutation).
func TestAdversarial_AppendOnlyGrantsDenyMutation(t *testing.T) {
	for _, h := range harnesses(t) {
		if h.dialect != "postgres" {
			continue
		}
		t.Run(h.dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, h.owner)
			eventID := seedEvent(t, h.app, tenant)
			auditID := seedAuditRow(t, h.app, tenant)

			// Control: the revoke is targeted, not blanket — the app role can
			// still UPDATE a non-protected table. If this fails the denials
			// below would be false positives (app can't update anything).
			if _, err := h.app.ExecContext(context.Background(),
				h.app.Rebind(`UPDATE tenants SET name = ? WHERE id = ?`), "renamed", string(tenant)); err != nil {
				t.Fatalf("control: app role UPDATE on non-protected table failed: %v", err)
			}

			attacks := []struct {
				name  string
				query string
				args  []any
			}{
				{"update events", `UPDATE events SET actor_id = ? WHERE id = ?`, []any{"attacker", eventID}},
				{"delete events", `DELETE FROM events WHERE id = ?`, []any{eventID}},
				{"update audit_log", `UPDATE audit_log SET actor_id = ? WHERE id = ?`, []any{"attacker", auditID}},
				{"delete audit_log", `DELETE FROM audit_log WHERE id = ?`, []any{auditID}},
			}
			for _, a := range attacks {
				err := mutate(h.app, tenant, a.query, a.args...)
				if err == nil {
					t.Errorf("%s: app role mutation succeeded, want permission denied by role separation", a.name)
					continue
				}
				// Must be denied by *lack of grant* (42501), not by the
				// trigger — otherwise this proves nothing about role
				// separation (see isInsufficientPrivilege).
				if !isInsufficientPrivilege(err) {
					t.Errorf("%s: want insufficient_privilege (42501) from role separation, got: %v", a.name, err)
				}
			}
		})
	}
}

// INV-E1 / INV-T4 defense in depth: even a connection that *holds* the grant
// (the owner on Postgres; the only connection on SQLite) is stopped by the
// append-only trigger, and the row is unchanged afterward.
func TestAdversarial_TriggerRejectsOwnerMutation(t *testing.T) {
	for _, h := range harnesses(t) {
		t.Run(h.dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, h.owner)
			eventID := seedEvent(t, h.app, tenant)
			auditID := seedAuditRow(t, h.app, tenant)

			attacks := []struct {
				name  string
				query string
				args  []any
			}{
				{"update events", `UPDATE events SET actor_id = ? WHERE id = ?`, []any{"attacker", eventID}},
				{"delete events", `DELETE FROM events WHERE id = ?`, []any{eventID}},
				{"update audit_log", `UPDATE audit_log SET actor_id = ? WHERE id = ?`, []any{"attacker", auditID}},
				{"delete audit_log", `DELETE FROM audit_log WHERE id = ?`, []any{auditID}},
			}
			for _, a := range attacks {
				err := mutate(h.owner, tenant, a.query, a.args...)
				if err == nil {
					t.Errorf("%s: owner mutation succeeded, want rejection by the append-only trigger", a.name)
					continue
				}
				// The owner holds every grant, so the only thing that can stop
				// it is the trigger — assert its message so a bypass that
				// errored for some *other* reason can't masquerade as success.
				if !strings.Contains(err.Error(), "append-only") {
					t.Errorf("%s: want append-only trigger rejection, got: %v", a.name, err)
				}
			}

			// History intact: the seeded event still reads back.
			var count int
			err := tenancy.WithTenant(context.Background(), h.app, tenant, func(ctx context.Context, tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, h.app.Rebind(`SELECT COUNT(*) FROM events WHERE id = ?`), eventID).Scan(&count)
			})
			if err != nil || count != 1 {
				t.Errorf("seeded event gone after rejected mutations: count=%d err=%v", count, err)
			}
		})
	}
}

// INV-T1: with tenant A's context set, RLS returns none of tenant B's rows
// (read isolation) and rejects an insert stamped with tenant B (write
// isolation, RLS WITH CHECK). Postgres-only: SQLite solo mode is one tenant
// per replica (ADR-005), so cross-tenant isolation is not part of its threat
// model and RLS does not apply there.
func TestAdversarial_CrossTenantIsolation(t *testing.T) {
	for _, h := range harnesses(t) {
		if h.dialect != "postgres" {
			continue
		}
		t.Run(h.dialect, func(t *testing.T) {
			tenantA := mustCreateTenant(t, h.owner)
			tenantB := mustCreateTenant(t, h.owner)
			seedEvent(t, h.app, tenantB) // one event owned by B

			// Read isolation: A's context sees zero of B's rows even with no
			// tenant predicate in the SQL.
			countUnder := func(tenant tenancy.ID) int {
				var n int
				err := tenancy.WithTenant(context.Background(), h.app, tenant, func(ctx context.Context, tx *sql.Tx) error {
					return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
				})
				if err != nil {
					t.Fatalf("count under %s: %v", tenant, err)
				}
				return n
			}
			if n := countUnder(tenantA); n != 0 {
				t.Errorf("tenant A sees %d rows, want 0 (cross-tenant read leak)", n)
			}
			if n := countUnder(tenantB); n != 1 {
				t.Errorf("tenant B sees %d of its own rows, want 1", n)
			}

			// Write isolation: under A's context, an insert stamped tenant B
			// is rejected by RLS WITH CHECK.
			err := mutate(h.app, tenantA, `INSERT INTO events
				(tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				string(tenantB), "s:x", 1, "evil.created", 1, `{}`, "attacker", idgen.New(), time.Now().UTC(), time.Now().UTC())
			if err == nil {
				t.Error("cross-tenant INSERT under tenant A's context succeeded, want RLS WITH CHECK rejection")
			}
		})
	}
}

// INV-E2: even a writer that skips the eventstore.Append pipeline and inserts
// raw SQL cannot duplicate a (tenant_id, stream_id, version) — the unique
// index holds the line, so optimistic concurrency can't be bypassed from
// below the app layer.
func TestAdversarial_RawInsertCannotDuplicateVersion(t *testing.T) {
	for _, h := range harnesses(t) {
		t.Run(h.dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, h.owner)
			stream := eventstore.StreamID("s:" + idgen.New())
			first, err := eventstore.Append(context.Background(), h.app, tenant, stream, 0, idgen.New(),
				eventstore.NewEvent{Type: "created", SchemaVersion: 1, Payload: []byte(`{}`), ActorID: "a"})
			if err != nil {
				t.Fatalf("seed append: %v", err)
			}

			// Raw insert reusing the same version as the committed event.
			err = mutate(h.app, tenant, `INSERT INTO events
				(tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				string(tenant), string(stream), first.Version, "dup.created", 1, `{}`, "attacker", idgen.New(), time.Now().UTC(), time.Now().UTC())
			if err == nil {
				t.Error("raw INSERT duplicating an existing version succeeded, want unique-index rejection (INV-E2)")
			}
		})
	}
}

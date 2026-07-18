//go:build integrity

// DB role-separation suite (docs/19 layer 3; INV-F5) — Postgres only. Proves
// the app role cannot write the event log outside the pipeline-owned functions,
// and that the balance / open-period checks are enforced by the database
// itself (not merely the Go pipeline), by calling ledger_post_entry directly
// with bad input. SQLite has no roles (ADR-005) — skipped there; its
// enforcement (Go pipeline + append-only trigger) is covered by
// post_integrity_test.go.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func isInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// INV-F5: a raw INSERT into events from the app role is denied by lack of grant
// (42501) — the log can only be written through the pipeline functions.
func TestAppRoleCannotWriteEventsDirectly(t *testing.T) {
	for dialect, db := range testDialects(t) {
		if dialect != "postgres" {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			// The pipeline path works (proven broadly by TestPostAndLoad under
			// the same locked-down role); here we show the *direct* path fails.
			err := tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, db.Rebind(`
					INSERT INTO events (tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
					string(f.tenant), "forged", 1, "ledger.entry.posted", 1, `{}`, "attacker", "forge-1", time.Now().UTC(), time.Now().UTC())
				return e
			})
			if !isInsufficientPrivilege(err) {
				t.Fatalf("direct INSERT on events: err = %v, want insufficient_privilege (42501)", err)
			}
		})
	}
}

// INV-F1 at the storage layer: ledger_post_entry rejects an unbalanced entry
// even when called directly (bypassing the Go pipeline's own balance check).
func TestLedgerPostEntryRejectsUnbalancedAtStorage(t *testing.T) {
	for dialect, db := range testDialects(t) {
		if dialect != "postgres" {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			payload := fmt.Sprintf(
				`{"period":"%s","currency":"USD","lines":[{"account_id":"%s","debit":100,"credit":0},{"account_id":"%s","debit":0,"credit":99}]}`,
				f.period, f.cash, f.revenue)
			err := callPostEntry(f.ctx, db, f.tenant, "s1", f.period, payload)
			if err == nil || !strings.Contains(err.Error(), "balance") {
				t.Fatalf("direct unbalanced ledger_post_entry: err = %v, want a balance error", err)
			}
		})
	}
}

// INV-F3 at the storage layer: ledger_post_entry rejects a closed period when
// called directly.
func TestLedgerPostEntryRejectsClosedPeriodAtStorage(t *testing.T) {
	for dialect, db := range testDialects(t) {
		if dialect != "postgres" {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			if err := ClosePeriod(f.ctx, db, f.tenant, f.periodID); err != nil {
				t.Fatalf("ClosePeriod: %v", err)
			}
			payload := fmt.Sprintf(
				`{"period":"%s","currency":"USD","lines":[{"account_id":"%s","debit":100,"credit":0},{"account_id":"%s","debit":0,"credit":100}]}`,
				f.period, f.cash, f.revenue)
			err := callPostEntry(f.ctx, db, f.tenant, "s2", f.period, payload)
			if err == nil || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("direct closed-period ledger_post_entry: err = %v, want a closed-period error", err)
			}
		})
	}
}

// callPostEntry invokes ledger_post_entry directly (the storage-layer path).
func callPostEntry(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream, period, payload string) error {
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var id int64
		var outStream string
		row := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT out_id, out_stream FROM ledger_post_entry(?, ?, ?, ?, ?, ?, ?)`),
			stream, period, payload, "actor", "cmd-"+stream, time.Now().UTC(), time.Now().UTC())
		return row.Scan(&id, &outStream)
	})
}

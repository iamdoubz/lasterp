// SPDX-License-Identifier: AGPL-3.0-only

// Package conformance is the shared adapter conformance suite (docs/11
// WP-0.2 AC): the exact same subtests run against both the Postgres and
// SQLite adapters so "identical suite passes on both" holds by
// construction, not by parallel-but-separate test files.
package conformance

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
)

// Run applies migrations against db and exercises every conformance
// subtest. db must point at an empty database/schema.
func Run(t *testing.T, db *storage.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("MigrateApplies", func(t *testing.T) { testMigrateApplies(t, ctx, db) })
	t.Run("MigrateIsIdempotent", func(t *testing.T) { testMigrateIdempotent(t, ctx, db) })
	t.Run("CRUDRoundTrip", func(t *testing.T) { testCRUDRoundTrip(t, ctx, db) })
	t.Run("UniqueConstraintViolation", func(t *testing.T) { testUniqueViolation(t, ctx, db) })
	t.Run("TransactionRollback", func(t *testing.T) { testTxRollback(t, ctx, db) })
	t.Run("NullRoundTrip", func(t *testing.T) { testNullRoundTrip(t, ctx, db) })
}

func testMigrateApplies(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	var n int
	row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n == 0 {
		t.Fatal("schema_migrations has no rows after Apply")
	}
}

func testMigrateIdempotent(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	var before int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("second migrate.Apply: %v", err)
	}
	var after int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if before != after {
		t.Fatalf("schema_migrations row count changed on re-apply: %d -> %d", before, after)
	}
}

func testCRUDRoundTrip(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	const tenant = "tenant-crud"

	insert := db.Rebind(`INSERT INTO storage_conformance_items (id, tenant_id, name, note) VALUES (?, ?, ?, ?)`)
	if _, err := db.ExecContext(ctx, insert, "item-1", tenant, "widget", "first"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var name, note string
	selectQ := db.Rebind(`SELECT name, note FROM storage_conformance_items WHERE id = ?`)
	if err := db.QueryRowContext(ctx, selectQ, "item-1").Scan(&name, &note); err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "widget" || note != "first" {
		t.Fatalf("select got (%q, %q), want (%q, %q)", name, note, "widget", "first")
	}

	update := db.Rebind(`UPDATE storage_conformance_items SET note = ? WHERE id = ?`)
	if _, err := db.ExecContext(ctx, update, "second", "item-1"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.QueryRowContext(ctx, selectQ, "item-1").Scan(&name, &note); err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if note != "second" {
		t.Fatalf("note after update = %q, want %q", note, "second")
	}

	del := db.Rebind(`DELETE FROM storage_conformance_items WHERE id = ?`)
	if _, err := db.ExecContext(ctx, del, "item-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err := db.QueryRowContext(ctx, selectQ, "item-1").Scan(&name, &note)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("select after delete: err = %v, want sql.ErrNoRows", err)
	}
}

func testUniqueViolation(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	const tenant = "tenant-unique"
	insert := db.Rebind(`INSERT INTO storage_conformance_items (id, tenant_id, name, note) VALUES (?, ?, ?, ?)`)

	if _, err := db.ExecContext(ctx, insert, "u-1", tenant, "gadget", ""); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	defer db.ExecContext(ctx, db.Rebind(`DELETE FROM storage_conformance_items WHERE id = ?`), "u-1")

	_, err := db.ExecContext(ctx, insert, "u-2", tenant, "gadget", "")
	if err == nil {
		t.Fatal("duplicate (tenant_id, name) insert succeeded, want unique constraint violation")
	}
}

func testTxRollback(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	insert := db.Rebind(`INSERT INTO storage_conformance_items (id, tenant_id, name, note) VALUES (?, ?, ?, ?)`)
	if _, err := tx.ExecContext(ctx, insert, "rb-1", "tenant-rb", "rollback-me", ""); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var n int
	row := db.QueryRowContext(ctx, db.Rebind(`SELECT COUNT(*) FROM storage_conformance_items WHERE id = ?`), "rb-1")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if n != 0 {
		t.Fatalf("row visible after rollback: count = %d, want 0", n)
	}
}

func testNullRoundTrip(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	insert := db.Rebind(`INSERT INTO storage_conformance_items (id, tenant_id, name, note) VALUES (?, ?, ?, ?)`)
	if _, err := db.ExecContext(ctx, insert, "null-1", "tenant-null", "nullable", nil); err != nil {
		t.Fatalf("insert with NULL note: %v", err)
	}
	defer db.ExecContext(ctx, db.Rebind(`DELETE FROM storage_conformance_items WHERE id = ?`), "null-1")

	var note sql.NullString
	row := db.QueryRowContext(ctx, db.Rebind(`SELECT note FROM storage_conformance_items WHERE id = ?`), "null-1")
	if err := row.Scan(&note); err != nil {
		t.Fatalf("select NULL note: %v", err)
	}
	if note.Valid {
		t.Fatalf("note.Valid = true, want NULL (got %q)", note.String)
	}
}

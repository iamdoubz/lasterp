// SPDX-License-Identifier: AGPL-3.0-only

package faultinject

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// TestFaultLandsMidScan is the wrapper's own check: the fault must arrive
// after a row has been yielded (so an accumulating callback has something to
// duplicate), must look like SQLITE_BUSY to storage.IsBusy (so WithTenant
// retries rather than propagating), and must not fire twice (so the retry
// succeeds).
func TestFaultLandsMidScan(t *testing.T) {
	ctx := context.Background()
	db, inj, err := Open(filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO t (id) VALUES (?)`, i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	read := func() (int, error) {
		rows, err := db.QueryContext(ctx, `SELECT id FROM t ORDER BY id`)
		if err != nil {
			return 0, err
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}

	inj.FailScan("FROM t", 1)
	n, err := read()
	if !storage.IsBusy(err) {
		t.Fatalf("first read: got (%d, %v), want a busy error storage.IsBusy recognises", n, err)
	}
	if n != 1 {
		t.Fatalf("fault landed after %d rows, want 1 — a fault before the first row leaves nothing accumulated", n)
	}
	if !inj.Fired() {
		t.Fatal("Fired() is false after the fault fired")
	}

	// Disarmed: the retry a caller would make has to succeed, or the injected
	// fault is a permanent failure rather than transient contention.
	if n, err := read(); err != nil || n != 3 {
		t.Fatalf("retry: got (%d, %v), want (3, nil)", n, err)
	}
}

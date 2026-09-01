//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package changefeed

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/faultinject"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
)

// TestReadObservesEachEntryExactlyOnceAcrossARetry is INV-S5 across a
// SQLITE_BUSY retry: "for any cursor position a reader observes every
// committed entry exactly once".
//
// Exactly once is the whole assertion. tenancy.WithTenant retries the entire
// callback on BUSY, so a Read that accumulated into a slice captured from the
// enclosing scope returned the first attempt's rows *plus* the second's — the
// same committed entry observed twice, with no error and no log line to say
// so. Every consumer of this feed trusts the promise: the replica, the plugin
// hook runner, the automation runner.
//
// SQLite only, and not because the harness is lazy: storage.IsBusy matches
// "database is locked", which Postgres never produces, so there is no retry to
// alias on that dialect (WP-3.3d-decisions.md §2).
func TestReadObservesEachEntryExactlyOnceAcrossARetry(t *testing.T) {
	ctx := context.Background()
	db, inj := faultDB(t)
	tenant := mustCreateTenant(t, db)

	const entries = 5
	for i := 0; i < entries; i++ {
		appendCommitted(t, db, tenant, refFor(i))
	}

	want, err := Read(ctx, db, tenant, 0, 100, nil)
	if err != nil {
		t.Fatalf("uncontended read: %v", err)
	}
	if len(want) != entries {
		t.Fatalf("uncontended read returned %d entries, want %d", len(want), entries)
	}

	// Fail partway through the scan: rows already appended, more still to come.
	inj.FailScan("FROM change_feed", 2)
	got, err := Read(ctx, db, tenant, 0, 100, nil)
	if err != nil {
		t.Fatalf("read across a retry: %v", err)
	}
	if !inj.Fired() {
		t.Fatal("the injected fault never fired, so the comparison below proves nothing")
	}

	if len(got) != len(want) {
		t.Fatalf("read across a retry returned %d entries, want exactly %d — INV-S5 requires "+
			"every committed entry to be observed exactly once, and a retried callback that "+
			"accumulates into a captured slice returns the first attempt's rows plus the second's",
			len(got), len(want))
	}
	for i := range want {
		if got[i].Cursor != want[i].Cursor {
			t.Fatalf("entry %d: got cursor %d, want %d — the retried read is not the uncontended read",
				i, got[i].Cursor, want[i].Cursor)
		}
	}
}

// faultDB is testSQLiteDB with the fault-injecting driver underneath.
func faultDB(t *testing.T) (*storage.DB, *faultinject.Injector) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "changefeed.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)"
	db, inj, err := faultinject.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := migrate.Apply(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db, inj
}

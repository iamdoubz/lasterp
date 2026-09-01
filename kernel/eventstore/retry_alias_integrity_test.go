//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package eventstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/faultinject"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
)

// TestFeedReadsAreUnchangedByARetry is INV-E5 across a SQLITE_BUSY retry:
// rebuild(events) ≡ projection only holds if the same log reads the same way
// every time. A retried callback that kept the failed attempt's rows made the
// *same* log fold differently on two reads, and money paths are downstream of
// that fold.
//
// TestFeedReplayDeterminism already asserts two uncontended reads agree; this
// asserts a read interrupted mid-scan agrees with them too. SQLite only —
// storage.IsBusy matches "database is locked", which Postgres never produces
// (WP-3.3d-decisions.md §2).
func TestFeedReadsAreUnchangedByARetry(t *testing.T) {
	ctx := context.Background()
	db, inj := faultDB(t)
	tenant := mustCreateTenant(t, db)
	stream := StreamID("invoice:" + idgen.New())

	const events = 6
	for i := 1; i <= events; i++ {
		if _, err := Append(ctx, db, tenant, stream, i-1, idgen.New(), NewEvent{
			Type: "invoice.line_added", SchemaVersion: 1,
			Payload: json.RawMessage(`{}`), ActorID: "user-1",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	t.Run("ReadFeed", func(t *testing.T) {
		want, err := ReadFeed(ctx, db, tenant, 0, 100)
		if err != nil {
			t.Fatalf("uncontended read: %v", err)
		}
		if len(want) != events {
			t.Fatalf("uncontended read returned %d events, want %d", len(want), events)
		}

		inj.FailScan("FROM events", 2)
		got, err := ReadFeed(ctx, db, tenant, 0, 100)
		if err != nil {
			t.Fatalf("read across a retry: %v", err)
		}
		assertFired(t, inj)
		assertSameEvents(t, got, want)
	})

	t.Run("LoadStream", func(t *testing.T) {
		_, want, err := LoadStream(ctx, db, tenant, stream, nil)
		if err != nil {
			t.Fatalf("uncontended load: %v", err)
		}
		if len(want) != events {
			t.Fatalf("uncontended load returned %d events, want %d", len(want), events)
		}

		inj.FailScan("FROM events", 2)
		_, got, err := LoadStream(ctx, db, tenant, stream, nil)
		if err != nil {
			t.Fatalf("load across a retry: %v", err)
		}
		assertFired(t, inj)
		assertSameEvents(t, got, want)
	})
}

func assertFired(t *testing.T, inj *faultinject.Injector) {
	t.Helper()
	if !inj.Fired() {
		t.Fatal("the injected fault never fired, so the comparison that follows proves nothing")
	}
}

func assertSameEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read across a retry returned %d events, want exactly %d — a projection folding "+
			"the extra copies double-counts, and INV-E5 makes the fold a pure function of the log",
			len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("event %d: got id %d, want %d — the retried read is not the uncontended read",
				i, got[i].ID, want[i].ID)
		}
	}
}

// faultDB is testSQLiteDB with the fault-injecting driver underneath.
func faultDB(t *testing.T) (*storage.DB, *faultinject.Injector) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "eventstore.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)"
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

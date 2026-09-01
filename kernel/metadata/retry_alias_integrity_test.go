//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/faultinject"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
)

// TestReadsAreUnchangedByARetry covers the widest blast radius of the WP-3.3d
// aliasing defect: every generic list, page and multi-get in the product goes
// through these three methods, so a duplicated row here is a duplicated row in
// the UI, the API and the MCP surface at once.
//
// The property is "a retried read returns what one read returns". WithTenant
// re-runs the whole callback on SQLITE_BUSY, so a callback accumulating into a
// slice it captured returned the failed attempt's rows plus the good one's.
// SQLite only: storage.IsBusy matches "database is locked", which Postgres
// never produces (WP-3.3d-decisions.md §2).
func TestReadsAreUnchangedByARetry(t *testing.T) {
	db, inj := faultDB(t)
	tenant := mustCreateTenant(t, db)
	crud := buildContactCRUD(t, db, tenant)
	ctx := actorWithPermissions(t, db, tenant, "create", "read")

	const records = 5
	ids := make([]string, 0, records)
	for i := 0; i < records; i++ {
		rec, err := crud.Create(ctx, db, tenant, Record{
			"full_name": "Contact " + string(rune('A'+i)), "email": string(rune('a'+i)) + "@example.com",
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		id, _ := rec["id"].(string)
		ids = append(ids, id)
	}
	match := "FROM " + TableName("Contact")

	for _, tc := range []struct {
		name string
		read func() ([]Record, error)
	}{
		{"List", func() ([]Record, error) { return crud.List(ctx, db, tenant) }},
		{"ListPage", func() ([]Record, error) { return crud.ListPage(ctx, db, tenant, "", 100) }},
		{"GetMany", func() ([]Record, error) { return crud.GetMany(ctx, db, tenant, ids) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := tc.read()
			if err != nil {
				t.Fatalf("uncontended read: %v", err)
			}
			if len(want) != records {
				t.Fatalf("uncontended read returned %d records, want %d", len(want), records)
			}

			inj.FailScan(match, 2)
			got, err := tc.read()
			if err != nil {
				t.Fatalf("read across a retry: %v", err)
			}
			if !inj.Fired() {
				t.Fatal("the injected fault never fired, so the comparison below proves nothing")
			}
			if len(got) != len(want) {
				t.Fatalf("read across a retry returned %d records, want exactly %d — a retried "+
					"callback that accumulates into a captured slice keeps the failed attempt's rows",
					len(got), len(want))
			}
			for i := range want {
				if got[i]["id"] != want[i]["id"] {
					t.Fatalf("record %d: got id %v, want %v — the retried read is not the uncontended read",
						i, got[i]["id"], want[i]["id"])
				}
			}
		})
	}
}

// faultDB is testSQLiteDB with the fault-injecting driver underneath.
func faultDB(t *testing.T) (*storage.DB, *faultinject.Injector) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "metadata.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)"
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

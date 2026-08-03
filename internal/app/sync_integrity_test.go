//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-2.2a: the server half of the client replica — resolving change-feed
// pointers into row state, and paging a snapshot to hydrate from.
//
// Invariants: INV-T1 (a replica never receives another tenant's rows) and
// INV-T2 (materialisation re-authorises per object kind rather than inheriting
// the feed's single sync:read grant). INV-S3 is deliberately not claimed here —
// convergence is a property of a replica, and there is no replica until
// WP-2.2b (decisions §0).

// createContact posts one contact and returns its id.
func createContact(t *testing.T, e *env, name string) string {
	t.Helper()
	status, body, rec := e.post("/api/v1/contact", map[string]any{
		"name": name, "email": idgen.New() + "@example.test", "kind": "customer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create contact = %d; body=%s", status, body)
	}
	return mustField(t, rec, "id")
}

// changesWithRows reads the feed from cursor 0 with row state included.
func changesWithRows(t *testing.T, e *env) (pointers []any, rows map[string]any) {
	t.Helper()
	status, body, parsed := e.get("/api/v1/sync/changes?include=rows&limit=1000")
	if status != http.StatusOK {
		t.Fatalf("sync/changes = %d; body=%s", status, body)
	}
	pointers, _ = parsed["data"].([]any)
	rows, _ = parsed["rows"].(map[string]any)
	return pointers, rows
}

// rowsFor returns the materialised rows for one object.
func rowsFor(rows map[string]any, object string) []any {
	got, _ := rows[object].([]any)
	return got
}

// TestMaterializedChangesCarryRowState is the core of WP-2.2a: the feed on disk
// holds pointers (WP-2.1 §3), and a replica cannot apply a pointer. With
// include=rows the same request also carries the rows those pointers name.
func TestMaterializedChangesCarryRowState(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			id := createContact(t, e, "Materialise Me")

			pointers, rows := changesWithRows(t, e)
			if len(pointers) == 0 {
				t.Fatal("feed returned no pointers after a contact was created")
			}

			contacts := rowsFor(rows, "Contact")
			if len(contacts) == 0 {
				t.Fatalf("no Contact rows materialised; rows=%v", rows)
			}
			var found map[string]any
			for _, r := range contacts {
				rec, _ := r.(map[string]any)
				if rec["id"] == id {
					found = rec
				}
			}
			if found == nil {
				t.Fatalf("contact %s absent from materialised rows %v", id, contacts)
			}
			if found["name"] != "Materialise Me" {
				t.Fatalf("materialised row carries name %v, want the row's actual state", found["name"])
			}
		})
	}
}

// A row touched several times in one page resolves once, not once per entry:
// the feed says what changed, and the answer to "what is it now" is the same
// row however many times it was asked about.
func TestMaterializationDeduplicatesRepeatedChanges(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			id := createContact(t, e, "Edited Thrice")
			for i := range 3 {
				status, body, _ := e.call("PATCH", "/api/v1/contact/"+id, e.token, idgen.New(),
					map[string]any{"name": fmt.Sprintf("Edit %d", i)})
				if status != http.StatusOK {
					t.Fatalf("patch %d = %d; body=%s", i, status, body)
				}
			}

			_, rows := changesWithRows(t, e)
			var seen int
			for _, r := range rowsFor(rows, "Contact") {
				rec, _ := r.(map[string]any)
				if rec["id"] == id {
					seen++
				}
			}
			if seen != 1 {
				t.Fatalf("contact appears %d times in materialised rows, want exactly 1", seen)
			}
		})
	}
}

// INV-T2: materialisation re-authorises per object kind. WP-2.1's threat notes
// leaned on the pointer design to bound an over-broad sync:read to names and
// ids; this is the test that the bound is re-established now that rows travel.
func TestMaterializationRefusesObjectsTheCallerCannotRead(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			createContact(t, e, "Secret Customer")

			// A principal who may read the feed but not contacts.
			feedOnly := e.issueUser(t, map[string][]string{"sync": {"read"}})
			status, body, parsed := e.call("GET", "/api/v1/sync/changes?include=rows&limit=1000", feedOnly, "", nil)
			if status != http.StatusOK {
				t.Fatalf("sync/changes as feed-only user = %d; body=%s", status, body)
			}

			if pointers, _ := parsed["data"].([]any); len(pointers) == 0 {
				t.Fatal("feed-only principal got no pointers; the feed grant should still work")
			}
			rows, _ := parsed["rows"].(map[string]any)
			if got := rowsFor(rows, "Contact"); len(got) != 0 {
				t.Fatalf("caller without Contact:read received %d contact row(s): %v", len(got), got)
			}
		})
	}
}

// A deletion must reach the replica. CRUD soft-deletes and List hides archived
// rows, which is right for a screen and a permanent divergence for a replica:
// a client holding the row and told nothing keeps it forever (decisions §5).
func TestMaterializationConveysDeletes(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			id := createContact(t, e, "Doomed")

			status, body, _ := e.call("DELETE", "/api/v1/contact/"+id, e.token, idgen.New(), nil)
			if status != http.StatusOK && status != http.StatusNoContent {
				t.Fatalf("delete contact = %d; body=%s", status, body)
			}

			_, rows := changesWithRows(t, e)
			var rec map[string]any
			for _, r := range rowsFor(rows, "Contact") {
				m, _ := r.(map[string]any)
				if m["id"] == id {
					rec = m
				}
			}
			if rec == nil {
				t.Fatalf("deleted contact %s was omitted from materialised rows; "+
					"a replica holding it would never learn it is gone", id)
			}
			if rec["archived_at"] == nil {
				t.Fatalf("deleted contact carries no archived_at: %v", rec)
			}
		})
	}
}

// Hydration: snapshot pages by id, reports the feed position to resume from,
// and terminates.
func TestSnapshotPagesAndReportsCursor(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			const total = 7
			want := map[string]bool{}
			for i := range total {
				want[createContact(t, e, fmt.Sprintf("Contact %d", i))] = true
			}

			got := map[string]bool{}
			after, pages := "", 0
			for {
				pages++
				if pages > total+2 {
					t.Fatal("snapshot paging did not terminate")
				}
				status, body, parsed := e.get("/api/v1/sync/snapshot?object=Contact&limit=3&after=" + after)
				if status != http.StatusOK {
					t.Fatalf("snapshot = %d; body=%s", status, body)
				}
				if cursor, _ := parsed["cursor"].(float64); cursor <= 0 {
					t.Fatalf("snapshot reported cursor %v; a hydrating client has nowhere to resume", parsed["cursor"])
				}
				rows, _ := parsed["data"].([]any)
				for _, r := range rows {
					rec, _ := r.(map[string]any)
					id, _ := rec["id"].(string)
					if got[id] {
						t.Fatalf("row %s appeared on two pages", id)
					}
					got[id] = true
				}
				next, _ := parsed["next"].(string)
				if next == "" {
					break
				}
				after = next
			}

			if len(got) != total {
				t.Fatalf("snapshot returned %d rows over %d pages, want %d", len(got), pages, total)
			}
			for id := range want {
				if !got[id] {
					t.Fatalf("snapshot missed contact %s", id)
				}
			}
		})
	}
}

// A fresh replica never held an archived row, so it does not need to be told
// the row is gone — the snapshot omits it (decisions §4/§5).
func TestSnapshotExcludesArchived(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			keep := createContact(t, e, "Kept")
			gone := createContact(t, e, "Archived")
			if status, body, _ := e.call("DELETE", "/api/v1/contact/"+gone, e.token, idgen.New(), nil); status >= 300 {
				t.Fatalf("delete = %d; body=%s", status, body)
			}

			status, body, parsed := e.get("/api/v1/sync/snapshot?object=Contact&limit=100")
			if status != http.StatusOK {
				t.Fatalf("snapshot = %d; body=%s", status, body)
			}
			rows, _ := parsed["data"].([]any)
			var sawKeep bool
			for _, r := range rows {
				rec, _ := r.(map[string]any)
				if rec["id"] == gone {
					t.Fatalf("snapshot included an archived row: %v", rec)
				}
				if rec["id"] == keep {
					sawKeep = true
				}
			}
			if !sawKeep {
				t.Fatalf("snapshot omitted the live contact %s", keep)
			}
		})
	}
}

// INV-T1: one tenant's replica never receives another's rows. This is the
// surface whose whole job is handing data to a device that then holds it
// offline, so a leak here leaves the building.
func TestSyncSurfaceIsTenantScoped(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			a := seed(t, db)
			b := seed(t, db)
			idA := createContact(t, a, "Tenant A Customer")
			createContact(t, b, "Tenant B Customer")

			_, rowsB := changesWithRows(t, b)
			for _, r := range rowsFor(rowsB, "Contact") {
				rec, _ := r.(map[string]any)
				if rec["id"] == idA {
					t.Fatalf("tenant B's feed materialised tenant A's contact: %v", rec)
				}
				if rec["name"] == "Tenant A Customer" {
					t.Fatalf("tenant B's feed carried tenant A's data: %v", rec)
				}
			}

			status, body, parsed := b.get("/api/v1/sync/snapshot?object=Contact&limit=100")
			if status != http.StatusOK {
				t.Fatalf("snapshot = %d; body=%s", status, body)
			}
			rows, _ := parsed["data"].([]any)
			for _, r := range rows {
				rec, _ := r.(map[string]any)
				if rec["id"] == idA {
					t.Fatalf("tenant B's snapshot returned tenant A's contact: %v", rec)
				}
			}
		})
	}
}

// An object with no replicable surface is a 404, not an empty page: a client
// hydrating a name the server does not replicate should learn that, rather
// than conclude the table is empty and converge on nothing.
func TestSnapshotRefusesUnknownObject(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if status, _, _ := e.get("/api/v1/sync/snapshot?object=NoSuchThing&limit=10"); status != http.StatusNotFound {
				t.Fatalf("snapshot of an unknown object = %d, want 404", status)
			}
			if status, _, _ := e.get("/api/v1/sync/snapshot?limit=10"); status != http.StatusBadRequest {
				t.Fatalf("snapshot without an object = %d, want 400", status)
			}
		})
	}
}

// Without include=rows the response is exactly what WP-2.1 shipped — pointers
// and a cursor, no row state. The materialising path is opt-in, so a client
// that only wants to know something moved does not pay for the join.
func TestChangesWithoutIncludeCarryNoRows(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			createContact(t, e, "Pointer Only")

			status, body, parsed := e.get("/api/v1/sync/changes?limit=1000")
			if status != http.StatusOK {
				t.Fatalf("sync/changes = %d; body=%s", status, body)
			}
			if _, present := parsed["rows"]; present {
				t.Fatalf("plain feed read carried a rows key: %v", parsed["rows"])
			}
			if pointers, _ := parsed["data"].([]any); len(pointers) == 0 {
				t.Fatal("plain feed read returned no pointers")
			}
		})
	}
}

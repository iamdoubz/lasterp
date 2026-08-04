//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"sort"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-2.4: scope management — role-based scope computation, re-shape on change,
// revocation purge. The AC is "entitlement-change scenarios", and these are
// they: an entitlement is withdrawn or granted between two syncs of the same
// replica, and the replica's shape has to follow.
//
// Invariants carried here:
//
//   - INV-T1 / INV-T2 — the feed is filtered to what the principal may read,
//     pointers included. WP-2.1's threat notes leaned on pointers being a
//     weaker disclosure than rows to justify an unfiltered feed under one
//     sync:read grant; this is what removes the need for that argument.
//   - INV-S1 — a revocation under queued work loses none of it. The purge takes
//     the server's data and never the user's (WP-2.4-decisions.md §5).
//   - INV-S3 — convergence, now to the *scoped* projection: exactly the
//     in-scope rows, no more and no fewer.
//   - INV-S4 — a rejection caused by revocation is surfaced with the server's
//     own reason, for an object whose rows are gone.
//   - INV-S5 — a filtered page still advances the cursor past what it filtered,
//     so no reader re-scans for ever and none stops short of what it can see.

// scopedGrants is a principal who may read Contacts and follow the feed, and
// who may not read Accounts. Both objects are replicable and both are written
// by the fixtures below, so the difference between them is entitlement and
// nothing else.
func scopedGrants() map[string][]string {
	return map[string][]string{
		"Contact": {"create", "read", "update"},
		"Account": {"read"},
		"sync":    {"read"},
	}
}

// scopedUser creates a user with grants and returns both its id and a bearer
// token, because the entitlement-change scenarios need to revoke from a
// principal they are also driving a replica as.
func scopedUser(t *testing.T, e *env, grants map[string][]string) (identity.UserID, authz.RoleID, string) {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, e.db, e.tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, e.db, e.tenant, "scoped-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for object, actions := range grants {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, e.db, e.tenant, role, object, action, ""); err != nil {
				t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, e.db, e.tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	issued, err := identity.IssueSession(ctx, e.db, e.tenant, user.ID, "scope-test-device")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	return user.ID, role, issued.Token
}

// revoke withdraws one entitlement, which is the whole of an "entitlement
// change" as far as this WP is concerned.
func revoke(t *testing.T, db *storage.DB, tenant tenancy.ID, role authz.RoleID, object, action string) {
	t.Helper()
	if err := authz.RevokePermission(context.Background(), db, tenant, role, object, action); err != nil {
		t.Fatalf("RevokePermission(%s,%s): %v", object, action, err)
	}
}

func grant(t *testing.T, db *storage.DB, tenant tenancy.ID, role authz.RoleID, object, action string) {
	t.Helper()
	if err := authz.GrantPermission(context.Background(), db, tenant, role, object, action, ""); err != nil {
		t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
	}
}

// seedScopeFixture writes rows of both kinds, so a filtered feed has something
// to filter and a purge has something to delete.
func seedScopeFixture(t *testing.T, e *env) {
	t.Helper()
	e.createAccount("4000-"+idgen.New()[:8], "Scope Revenue", "income")
	for i, name := range []string{"Scope Ada", "Scope Grace", "Scope Alan"} {
		status, body, _ := e.post("/api/v1/contact", map[string]any{
			"name": name, "email": idgen.New() + "@example.test", "kind": "customer",
		})
		if status != 201 {
			t.Fatalf("seed contact %d = %d; body=%s", i, status, body)
		}
	}
}

// --- 6 (first, per the premortem watch list): the purge must not eat the outbox ---

// TestPurgeNeverDeletesQueuedWork is the test WP-2.4 was told to write before
// any other: "a purge or re-shape must never delete a row id referenced by an
// undrained _outbox command".
//
// The reading taken is decisions §5 — the *command* is inviolable, the replica
// row is not. A revocation purge a queued command could veto would not be a
// revocation, and the row is a copy of something the server still has; the
// command body is the user's own work and nothing reconstructs it. So the
// property asserted is conservation of the queue across a revocation: every
// command that was queued when the entitlement was withdrawn ends up accepted
// by the server or visible in the tray, and none evaporates with the rows.
//
// The scenario is built so the purge runs while the outbox is **non-empty**,
// which is the only state in which this can be violated and is not the state a
// naive version of this test reaches. The drain runs before the re-shape, so
// with everything acceptable the queue is already empty by the time anything is
// purged and a purge that deleted `_outbox` outright would go unnoticed. Both
// `read` and `create` are therefore withdrawn: the drain stops at the first
// rejection (WP-2.3-decisions.md §9), leaving its successors queued across the
// purge, and it takes one further sync per command to settle them.
//
// Carries INV-S1 (no acknowledged write lost) and INV-S4 (no silent drop).
func TestPurgeNeverDeletesQueuedWork(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())

			run := newReplicaRun(t, e)
			run.token = token

			// A replica holding both objects, then work queued offline against
			// the one that is about to be revoked.
			before := run.syncWithArgs()
			if len(before["Contact"]) == 0 || len(before["Account"]) == 0 {
				t.Fatalf("fixture: replica should hold both objects, got %d contacts / %d accounts",
					len(before["Contact"]), len(before["Account"]))
			}
			queued := run.syncWithArgs("--offline", "--enqueue=3", "--label=Revoked")
			if got := len(queued["_outbox"]); got != 3 {
				t.Fatalf("expected 3 queued commands, got %d", got)
			}

			// The entitlement goes away while the work is still in the outbox.
			revoke(t, e.db, e.tenant, role, "Contact", "read")
			revoke(t, e.db, e.tenant, role, "Contact", "create")

			first := run.syncWithArgs()

			// The precondition that gives this test its teeth: the purge has
			// run, and it ran with commands still in the queue.
			if got := len(first["Contact"]); got != 0 {
				t.Fatalf("Contact should have been purged, %d rows remain", got)
			}
			// The drain stops at the first rejection, so after one sync exactly
			// one command is filed and the rest are still queued *across* the
			// purge. Stated as conservation rather than as a non-empty check so
			// that a purge which deleted the queue fails here saying so, rather
			// than looking like a scenario that never set itself up.
			if q, f := len(first["_outbox"]), len(first["_conflicts"]); q+f != 3 || q == 0 {
				t.Fatalf("the purge did not run over a live queue: %d queued + %d filed, "+
					"want 3 with at least one still queued — a purge that deleted _outbox "+
					"lands exactly here", q, f)
			}
			if held := heldObjects(t, first); contains(held, "Contact") {
				t.Errorf("Contact should have left _hydration, held = %v", held)
			}
			// Account, still in scope, is untouched — a purge that took
			// everything would satisfy the assertion above for the wrong reason.
			if got := len(first["Account"]); got == 0 {
				t.Errorf("Account is still in scope and should not have been purged")
			}

			// Now let the queue settle. One command leaves per sync, because
			// the drain stops at each rejection; the bound is what makes a
			// stalled queue a failure rather than a hang.
			dump := first
			for i := 0; i < 5 && len(dump["_outbox"]) > 0; i++ {
				dump = run.syncWithArgs()
			}

			// Conservation: three commands in, three outcomes out. Nothing
			// evaporated with the rows.
			accepted := serverCountNamed(t, e, "Revoked")
			filed := len(dump["_conflicts"])
			stillQueued := len(dump["_outbox"])
			if accepted+filed+stillQueued != 3 {
				t.Errorf("work was lost across the purge: %d accepted + %d filed + %d queued != 3",
					accepted, filed, stillQueued)
			}
			if stillQueued != 0 {
				t.Errorf("%d commands never reached a terminal state", stillQueued)
			}
			if got := len(dump["Contact"]); got != 0 {
				t.Errorf("Contact rows reappeared after the queue settled: %d", got)
			}
		})
	}
}

// TestTrayShowsWorkForAnObjectTheUserCanNoLongerRead is the second watch-list
// item. Materialisation returns nothing for a revoked object, so a tray built
// by joining conflicts to replica rows would render empty and look healthy
// while the user's work sat in it unseen.
//
// It is not built that way — `_conflicts` carries the command's own body and
// the server's problem+json (WP-2.3-decisions.md §1) — and that is exactly why
// it needs a test rather than a comment: nothing else would notice if a later
// change made the tray read the replica. Carries INV-S4.
func TestTrayShowsWorkForAnObjectTheUserCanNoLongerRead(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())

			run := newReplicaRun(t, e)
			run.token = token
			run.syncWithArgs()
			run.syncWithArgs("--offline", "--enqueue-invalid=1", "--label=Unreadable")

			// Revoke *create* as well, so the drain's rejection is the
			// revocation itself rather than the invalid payload: this is the
			// case where the user cannot read the object their work targets.
			revoke(t, e.db, e.tenant, role, "Contact", "read")
			revoke(t, e.db, e.tenant, role, "Contact", "create")

			after := run.syncWithArgs()

			if got := len(after["Contact"]); got != 0 {
				t.Fatalf("Contact should have been purged, %d rows remain", got)
			}
			conflicts := after["_conflicts"]
			if len(conflicts) != 1 {
				t.Fatalf("expected the refused command in the tray, got %d entries", len(conflicts))
			}

			// The two things the tray renders, both surviving the purge: the
			// user's own body and the server's own words.
			c := conflicts[0]
			body, ok := c["body"].(map[string]any)
			if !ok || body["name"] == nil {
				t.Errorf("the tray lost the user's own values: %v", c["body"])
			}
			if title, _ := c["title"].(string); title == "" {
				t.Errorf("the tray has no reason to show: %v", c)
			}
		})
	}
}

// --- scope computation ---

// TestScopeIsComputedFromRoleGrants is the AC's "role-based scope computation":
// the scope is the objects this principal may read, intersected with what is
// replicable and enabled — not the tenant's object list.
func TestScopeIsComputedFromRoleGrants(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, role, token := scopedUser(t, e, scopedGrants())

			scope := scopeOf(t, e, token)
			if !contains(scope, "Contact") || !contains(scope, "Account") {
				t.Fatalf("scope should hold both granted objects, got %v", scope)
			}
			// JournalEntry is granted nothing here, and is event-sourced
			// besides: a scope is over replicable objects (decisions §1).
			if contains(scope, "JournalEntry") {
				t.Errorf("scope should not carry an event-sourced object: %v", scope)
			}
			if contains(scope, "Invoice") {
				t.Errorf("scope should not carry an object this principal cannot read: %v", scope)
			}

			revoke(t, e.db, e.tenant, role, "Account", "read")

			narrowed := scopeOf(t, e, token)
			if contains(narrowed, "Account") {
				t.Errorf("revoking Account:read should narrow the scope, got %v", narrowed)
			}
			if !contains(narrowed, "Contact") {
				t.Errorf("revoking Account:read should not touch Contact, got %v", narrowed)
			}
		})
	}
}

// TestFeedIsFilteredToScope closes the seam WP-2.1 left open: an over-broad
// sync:read used to convey every object's *pointers*, bounded only by the
// argument that a pointer discloses less than a row. Carries INV-T1/INV-T2.
func TestFeedIsFilteredToScope(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())
			revoke(t, e.db, e.tenant, role, "Account", "read")

			status, body, parsed := e.call("GET", "/api/v1/sync/changes?after=0&limit=1000", token, "", nil)
			if status != 200 {
				t.Fatalf("GET changes = %d; body=%s", status, body)
			}
			entries, _ := parsed["data"].([]any)
			if len(entries) == 0 {
				t.Fatal("the feed should still carry the in-scope object")
			}
			sawContact := false
			for _, raw := range entries {
				entry, _ := raw.(map[string]any)
				key, _ := entry["scope_key"].(string)
				switch key {
				case "Contact":
					sawContact = true
				case "Account":
					t.Errorf("an out-of-scope pointer reached the client: %v", entry)
				}
			}
			if !sawContact {
				t.Error("the in-scope object was filtered out too")
			}
		})
	}
}

// TestSnapshotRefusesAnOutOfScopeObject: the purge is theatre if the client can
// simply re-hydrate what it was made to delete. Carries INV-T2.
func TestSnapshotRefusesAnOutOfScopeObject(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())

			status, _, _ := e.call("GET", "/api/v1/sync/snapshot?object=Account", token, "", nil)
			if status != 200 {
				t.Fatalf("snapshot before revocation = %d, want 200", status)
			}

			revoke(t, e.db, e.tenant, role, "Account", "read")

			status, body, _ := e.call("GET", "/api/v1/sync/snapshot?object=Account", token, "", nil)
			if status != 403 {
				t.Errorf("snapshot after revocation = %d, want 403; body=%s", status, body)
			}
		})
	}
}

// TestCursorAdvancesPastFilteredEntries is the INV-S5 half of the filter
// (decisions §7). A principal whose scope excludes everything in the feed must
// still be given a resume position past it: reporting the last *visible* entry
// would leave the client re-scanning the same range on every poll, and — since
// a short page means "caught up" — believing it was current while entries it
// could see lay beyond the cursor it kept.
func TestCursorAdvancesPastFilteredEntries(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			// sync:read and nothing else: entitled to follow the feed, entitled
			// to none of what is in it.
			_, _, token := scopedUser(t, e, map[string][]string{"sync": {"read"}})

			high, err := changefeed.HighWater(context.Background(), e.db, e.tenant)
			if err != nil {
				t.Fatalf("HighWater: %v", err)
			}
			if high == 0 {
				t.Fatal("fixture wrote nothing to the feed")
			}

			status, body, parsed := e.call("GET", "/api/v1/sync/changes?after=0&limit=1000", token, "", nil)
			if status != 200 {
				t.Fatalf("GET changes = %d; body=%s", status, body)
			}
			if entries, _ := parsed["data"].([]any); len(entries) != 0 {
				t.Fatalf("an empty scope should see nothing, got %d entries", len(entries))
			}
			cursor, _ := parsed["cursor"].(float64)
			if int64(cursor) != high {
				t.Errorf("cursor = %d, want the high-water mark %d — a filtered client "+
					"that keeps its old cursor re-scans the feed for ever", int64(cursor), high)
			}
		})
	}
}

// --- the re-shape ---

// TestRevocationPurgesTheReplica and TestNewEntitlementHydratesTheObject are
// the AC proper: the same replica file, an entitlement changed between two
// syncs, and the shape following in both directions. Carries INV-S3 —
// convergence to the *scoped* projection.
func TestRevocationPurgesTheReplica(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())

			run := newReplicaRun(t, e)
			run.token = token

			before := run.syncWithArgs()
			if len(before["Account"]) == 0 {
				t.Fatal("fixture: the replica should hold Accounts before the revocation")
			}
			contactsBefore := len(before["Contact"])
			if contactsBefore == 0 {
				t.Fatal("fixture: the replica should hold Contacts")
			}

			revoke(t, e.db, e.tenant, role, "Account", "read")

			after := run.syncWithArgs()
			if got := len(after["Account"]); got != 0 {
				t.Errorf("Account should have been purged, %d rows remain", got)
			}
			if held := heldObjects(t, after); contains(held, "Account") {
				t.Errorf("Account should have left _hydration, held = %v", held)
			}
			if got := len(after["Contact"]); got != contactsBefore {
				t.Errorf("Contact rows = %d, want %d — the purge took more than it was asked to",
					got, contactsBefore)
			}
		})
	}
}

func TestNewEntitlementHydratesTheObject(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			// Starts without Account:read, so the object is out of scope and
			// out of the replica.
			_, role, token := scopedUser(t, e, map[string][]string{
				"Contact": {"create", "read", "update"},
				"sync":    {"read"},
			})

			run := newReplicaRun(t, e)
			run.token = token

			before := run.syncWithArgs()
			if got := len(before["Account"]); got != 0 {
				t.Fatalf("Account is out of scope and should not be replicated, got %d rows", got)
			}

			grant(t, e.db, e.tenant, role, "Account", "read")

			after := run.syncWithArgs()
			if got := len(after["Account"]); got == 0 {
				t.Error("granting Account:read should have hydrated the object on the next sync")
			}
			if held := heldObjects(t, after); !contains(held, "Account") {
				t.Errorf("Account should have entered _hydration, held = %v", held)
			}
		})
	}
}

// TestReplicaConvergesToTheScopedProjection is INV-S3 restated for a narrowed
// principal: after a revocation the replica holds exactly the in-scope rows the
// server holds — not a subset (a purge that took too much) and not a superset
// (a purge that took too little).
func TestReplicaConvergesToTheScopedProjection(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedScopeFixture(t, e)
			_, role, token := scopedUser(t, e, scopedGrants())

			run := newReplicaRun(t, e)
			run.token = token
			run.syncWithArgs()

			revoke(t, e.db, e.tenant, role, "Account", "read")

			// More server-side work after the revocation, so the second sync is
			// a real catch-up over a filtered feed rather than a no-op.
			seedScopeFixture(t, e)
			dump := run.syncWithArgs()

			scope := scopeOf(t, e, token)
			for _, object := range scope {
				if len(dump[object]) == 0 {
					t.Errorf("%s is in scope but the replica holds none of it", object)
				}
			}
			for object, rows := range dump {
				if object == "_outbox" || object == "_conflicts" ||
					object == "_pending" || object == "_hydration" {
					continue
				}
				if !contains(scope, object) && len(rows) != 0 {
					t.Errorf("%s is out of scope but the replica holds %d rows", object, len(rows))
				}
			}

			// The oracle: in-scope rows match the server's own projection,
			// read straight from the database rather than through the sync
			// endpoints under test.
			if ok, detail := convergenceIn(t, e, dump, scope); !ok {
				t.Fatalf("the scoped replica diverged from the server projection:\n%s", detail)
			}
		})
	}
}

// --- helpers ---

// scopeOf reads GET /api/v1/sync/scope as the given principal.
func scopeOf(t *testing.T, e *env, token string) []string {
	t.Helper()
	status, body, parsed := e.call("GET", "/api/v1/sync/scope", token, "", nil)
	if status != 200 {
		t.Fatalf("GET scope = %d; body=%s", status, body)
	}
	raw, _ := parsed["data"].([]any)
	scope := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			scope = append(scope, s)
		}
	}
	sort.Strings(scope)
	return scope
}

// heldObjects reads the replica's own claim about what it replicates.
func heldObjects(t *testing.T, dump map[string][]map[string]any) []string {
	t.Helper()
	var held []string
	for _, row := range dump["_hydration"] {
		if object, ok := row["object"].(string); ok {
			held = append(held, object)
		}
	}
	sort.Strings(held)
	return held
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

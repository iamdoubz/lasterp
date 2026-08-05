// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.1b async delivery. The promise is docs/05's: at-least-once, with a dead
// letter for what fails every retry and nothing dropped silently — the shape
// INV-S4 gives rejected offline commands, for the same reason.

// runnerFor builds a runner over the Widget object.
func runnerFor(t *testing.T, db *storage.DB) *Runner {
	t.Helper()
	return NewRunner(Host{
		DB:      db,
		Objects: map[string]*metadata.CRUD{"Widget": widgetCRUD(t, db)},
		Limits:  Limits{MaxPages: 1024, Timeout: 5 * time.Second, MaxHostCalls: 1000},
	}, NewStats())
}

// writeWidget creates one Widget as a human actor and returns its id.
func writeWidget(t *testing.T, db *storage.DB, tenant tenancy.ID, name string) string {
	t.Helper()
	crud := widgetCRUD(t, db)
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})
	rec, err := crud.Create(ctx, db, tenant, metadata.Record{"name": name, "kind": "customer"})
	if err != nil {
		t.Fatalf("create widget: %v", err)
	}
	id, _ := rec["id"].(string)
	return id
}

func TestAsyncHookIsDeliveredForChangesAfterInstall(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			// A change from *before* the install: a plugin installed today does
			// not replay a tenant's history, or an install fires thousands of
			// hooks for changes nobody expected it to see.
			writeWidget(t, db, tenant, "Before Install")

			p := installHooks(t, db, tenant, "  - {event: Widget.changed, fn: note, mode: async}\n")
			runner := runnerFor(t, db)

			delivered, err := runner.Deliver(ctx, tenant)
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if delivered != 0 {
				t.Errorf("an install replayed %d historical changes", delivered)
			}

			id := writeWidget(t, db, tenant, "After Install")
			delivered, err = runner.Deliver(ctx, tenant)
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if delivered != 1 {
				t.Fatalf("delivered %d changes, want 1", delivered)
			}

			// The hook recorded the delivery in its own kv, which is both the
			// evidence and the idempotency pattern the host tells async authors
			// to use.
			value, found, err := kvGet(ctx, db, tenant, p.ID, "seen:Widget:"+id)
			if err != nil || !found {
				t.Fatalf("hook did not record its delivery (found=%v, err=%v)", found, err)
			}
			if value != "1" {
				t.Errorf("delivery count = %q, want 1", value)
			}

			// A second pass has nothing new: the cursor moved.
			if delivered, err = runner.Deliver(ctx, tenant); err != nil || delivered != 0 {
				t.Errorf("second pass delivered %d (err=%v), want 0 — the cursor did not advance", delivered, err)
			}
		})
	}
}

// TestRunnerResumesFromItsCursor is the crash story: a new runner over the same
// database picks up where the stored cursor left it, delivering what was missed
// and nothing that was already done.
func TestRunnerResumesFromItsCursor(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	installHooks(t, db, tenant, "  - {event: Widget.changed, fn: note, mode: async}\n")

	writeWidget(t, db, tenant, "One")
	if _, err := runnerFor(t, db).Deliver(ctx, tenant); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	writeWidget(t, db, tenant, "Two")
	writeWidget(t, db, tenant, "Three")

	// A different runner instance — the process restarted.
	delivered, err := runnerFor(t, db).Deliver(ctx, tenant)
	if err != nil {
		t.Fatalf("Deliver after restart: %v", err)
	}
	if delivered != 2 {
		t.Errorf("delivered %d after restart, want the 2 missed changes", delivered)
	}
}

// TestPluginDoesNotReactToItsOwnWrites is decisions §6. Without the actor on
// the feed entry the runner cannot tell a plugin's own output from anyone
// else's, and this hook — which writes a Widget every time a Widget changes —
// would deliver to itself forever.
func TestPluginDoesNotReactToItsOwnWrites(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.changed, fn: spawn, mode: async}\n")
	runner := runnerFor(t, db)

	writeWidget(t, db, tenant, "Trigger")

	// First pass: the human's write is delivered, and the hook writes one of
	// its own.
	delivered, err := runner.Deliver(ctx, tenant)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("first pass delivered %d, want 1", delivered)
	}

	// Second pass: the plugin's own write is in the feed and must be skipped.
	// Ten passes, so a loop would be unmistakable.
	total := 0
	for i := 0; i < 10; i++ {
		n, err := runner.Deliver(ctx, tenant)
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		total += n
	}
	if total != 0 {
		t.Fatalf("the plugin reacted to its own writes %d times — this is an infinite loop with a stopwatch on it", total)
	}

	// Non-vacuity: the plugin really did write, so there really was an entry to
	// skip. Without this a broken hook that wrote nothing would pass.
	crud := widgetCRUD(t, db)
	readCtx := actorCtx(t, db, tenant, [2]string{"Widget", "read"})
	rows, err := crud.List(readCtx, db, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var spawned int
	for _, r := range rows {
		if name, _ := r["name"].(string); strings.Contains(name, "spawned") {
			spawned++
		}
	}
	if spawned == 0 {
		t.Fatal("the hook never wrote anything, so nothing was suppressed and this test proves nothing")
	}
	if spawned > 1 {
		t.Errorf("the hook wrote %d records; suppression is leaking", spawned)
	}

	// And the feed really does carry the plugin as the author.
	changes, err := changefeed.Read(ctx, db, tenant, 0, 100, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var sawPluginActor bool
	for _, c := range changes {
		if c.ActorID == p.Principal() {
			sawPluginActor = true
		}
	}
	if !sawPluginActor {
		t.Error("no feed entry is attributed to the plugin; suppression is matching on nothing")
	}
}

// TestFailedDeliveryIsFiledNotDropped: a hook that fails every attempt puts the
// entry where a person can see it, and the cursor moves on — a queue head that
// retries forever blocks everything behind it, which is the stall INV-S4 counts
// as a silent drop.
func TestFailedDeliveryIsFiledNotDropped(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			p := installHooks(t, db, tenant, "  - {event: Widget.changed, fn: boom, mode: async}\n")
			runner := runnerFor(t, db)

			writeWidget(t, db, tenant, "Will Fail")
			if _, err := runner.Deliver(ctx, tenant); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			letters, err := DeadLetters(ctx, db, tenant, p.ID)
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 {
				t.Fatalf("dead letters = %d, want 1 — a failed delivery vanished", len(letters))
			}
			if letters[0].Fn != "boom" || letters[0].Object != "Widget" || letters[0].Attempts != DeliveryAttempts {
				t.Errorf("dead letter does not describe what failed: %+v", letters[0])
			}
			if letters[0].Error == "" {
				t.Error("a dead letter with no error tells an administrator nothing")
			}

			// The queue is not stuck behind it: the next change is delivered.
			writeWidget(t, db, tenant, "Also Fails")
			if _, err := runner.Deliver(ctx, tenant); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			letters, err = DeadLetters(ctx, db, tenant, p.ID)
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 2 {
				t.Errorf("dead letters = %d, want 2 — delivery stalled behind the first failure", len(letters))
			}
		})
	}
}

// A plugin with no async hooks is never asked for one, and a tenant with no
// plugins costs one query.
func TestDeliverIsANoOpWithoutAsyncHooks(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: veto, mode: sync}\n")

	writeWidget(t, db, tenant, "Sync Only")
	n, err := runnerFor(t, db).Deliver(ctx, tenant)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered %d to a plugin with no async hooks", n)
	}
}

// The feed's actor is what suppression matches on, so it must be recorded for
// ordinary human writes too — not just for plugins.
func TestFeedRecordsTheActorBehindAChange(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	actor := approver(t, db, tenant, [2]string{"Widget", "create"})
	crud := widgetCRUD(t, db)

	if _, err := crud.Create(authz.WithActor(ctx, actor), db, tenant,
		metadata.Record{"name": "By A Human", "kind": "customer"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	changes, err := changefeed.Read(ctx, db, tenant, 0, 10, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("no feed entries")
	}
	if changes[len(changes)-1].ActorID != string(actor.UserID) {
		t.Errorf("feed actor = %q, want the writing user", changes[len(changes)-1].ActorID)
	}
}

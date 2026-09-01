//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/outbound"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.3b's automation half. Invariants: **INV-T2/INV-T4** (an action writes
// through the ordinary gate, attributed to `automation:<id>` and never to the
// user whose write triggered it), **INV-T3** in ADR-022's shape (a condition
// only ever narrows — an unevaluable one does not fire) and INV-S5's cursor
// semantics reused from WP-3.1b.

// fakeObjects is an in-memory Objects, recording who wrote what. It stands in
// for CRUD because this package deliberately does not construct one — the
// interface is declared at the consumer side (runner.go).
type fakeObjects struct {
	mu      sync.Mutex
	records map[string]map[string]any
	writes  []write
	// feed, when set, publishes a change for every Update, which is what makes
	// the self-suppression test meaningful: without a feed entry there is
	// nothing for the automation to react to a second time.
	feed *storage.DB
	err  error
}

type write struct {
	object, id string
	actor      string
	changes    map[string]any
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{records: map[string]map[string]any{}}
}

func (f *fakeObjects) Get(_ context.Context, _ tenancy.ID, object, id string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[object+"/"+id]
	if !ok {
		return nil, nil
	}
	clone := make(map[string]any, len(rec))
	for k, v := range rec {
		clone[k] = v
	}
	return clone, nil
}

func (f *fakeObjects) Update(ctx context.Context, tenant tenancy.ID, actor authz.Actor, object, id string, changes map[string]any) error {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return err
	}
	rec := f.records[object+"/"+id]
	for k, v := range changes {
		rec[k] = v
	}
	f.writes = append(f.writes, write{object: object, id: id, actor: string(actor.UserID), changes: changes})
	feed := f.feed
	f.mu.Unlock()

	if feed != nil {
		// Published with the automation as the actor, exactly as CRUD would:
		// the write really was the automation's, and that attribution is what
		// self-suppression depends on.
		if err := appendFeed(ctx, feed, tenant, object, id, string(actor.UserID)); err != nil {
			return err
		}
	}
	return nil
}

// appendFeed publishes a change entry attributed to actor.
func appendFeed(ctx context.Context, db *storage.DB, tenant tenancy.ID, object, refID, actor string) error {
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return changefeed.Append(ctx, tx, db, changefeed.Entry{
			TenantID:   tenant,
			Object:     object,
			RefID:      refID,
			ScopeKey:   changefeed.ScopeKeyFor(object),
			Source:     changefeed.SourceAudit,
			ActorID:    actor,
			RecordedAt: time.Now().UTC(),
		})
	})
}

func (f *fakeObjects) seen() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]write(nil), f.writes...)
}

// fakePlugins records call_plugin invocations.
type fakePlugins struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakePlugins) Enqueue(_ context.Context, _ tenancy.ID, plugin, fn string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, plugin+"."+fn)
	return nil
}

func (f *fakePlugins) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func saveAutomation(t *testing.T, db *storage.DB, tenant tenancy.ID, src string) *Definition {
	t.Helper()
	d, err := Save(context.Background(), db, tenant, []byte(src), automationApprover(t, db, tenant))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return d
}

// An automation fires only when its condition matches, writes as its own
// principal, and records what it did.
func TestAutomationFiresOnlyWhenTheConditionMatches(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			objects.records["Invoice/big"] = map[string]any{"status": "posted", "total_minor": 250000}
			objects.records["Invoice/small"] = map[string]any{"status": "posted", "total_minor": 5}

			d := saveAutomation(t, db, tenant, `
id: nudge
name: Nudge big posted invoices
trigger:
  object: Invoice
condition: 'record.status == "posted" && record.total_minor > 100000'
actions:
  - type: field_update
    set:
      followup: true
`)
			publish(t, db, tenant, "Invoice", "big", "user-1")
			publish(t, db, tenant, "Invoice", "small", "user-1")

			r := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{})
			fired, err := r.RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 1 {
				t.Fatalf("fired %d times, want 1 — only the big invoice matches", fired)
			}

			writes := objects.seen()
			if len(writes) != 1 || writes[0].id != "big" {
				t.Fatalf("writes = %+v, want one write to the big invoice", writes)
			}
			// INV-T4: the automation, never the user whose write triggered it.
			if writes[0].actor != d.Principal() {
				t.Fatalf("write attributed to %q, want the automation principal %q",
					writes[0].actor, d.Principal())
			}

			// Both outcomes are recorded, so an operator can answer "why did
			// this invoice change" *and* "why did that one not".
			runs, err := Runs(ctx, db, tenant, d.ID, 0)
			if err != nil {
				t.Fatalf("Runs: %v", err)
			}
			var matched, skipped int
			for _, run := range runs {
				switch run.Outcome {
				case OutcomeMatched:
					matched++
				case OutcomeSkipped:
					skipped++
				}
			}
			if matched != 1 || skipped != 1 {
				t.Fatalf("runs: %d matched, %d skipped; want 1 and 1 (%+v)", matched, skipped, runs)
			}
		})
	}
}

// **The loop guard.** An automation whose action writes the object it
// subscribes to would react to its own write forever. Self-suppression is the
// same mechanism WP-3.1b built for plugins, and it is the reason change_feed
// carries an actor at all.
//
// The non-vacuity half matters as much: the automation must be proven to have
// really written something for the suppression to be suppressing anything.
func TestAutomationDoesNotReactToItsOwnWrites(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			objects.feed = db
			objects.records["Invoice/x"] = map[string]any{"status": "posted"}

			d := saveAutomation(t, db, tenant, `
id: loop
name: Would loop without suppression
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      touched_by_automation: "yes"
`)
			publish(t, db, tenant, "Invoice", "x", "user-1")

			r := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{})
			total := 0
			for pass := 0; pass < 10; pass++ {
				n, err := r.RunOnce(ctx, tenant)
				if err != nil {
					t.Fatalf("RunOnce (pass %d): %v", pass, err)
				}
				total += n
			}

			// Non-vacuity: it really did write, so there really was a feed entry
			// of its own to suppress.
			writes := objects.seen()
			if len(writes) == 0 {
				t.Fatal("the automation never wrote anything; suppression is not being tested")
			}
			if writes[0].actor != d.Principal() {
				t.Fatalf("write actor = %q, want %q", writes[0].actor, d.Principal())
			}
			// And it fired exactly once, over ten passes.
			if total != 1 {
				t.Fatalf("fired %d times over 10 passes, want 1 — the automation is reacting to itself", total)
			}
			if len(writes) != 1 {
				t.Fatalf("wrote %d times, want 1: %+v", len(writes), writes)
			}
		})
	}
}

// ADR-022's rule, through the automation path: an unevaluable condition does
// not fire. The action is a *write*, so acting when the rule could not be
// evaluated is the failure mode worth engineering against.
func TestUnevaluableConditionDoesNotFire(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			// The record lacks the field the condition reads, so evaluation
			// errors — which must deny.
			objects.records["Invoice/x"] = map[string]any{"status": "posted"}

			saveAutomation(t, db, tenant, `
id: missing-field
name: Reads a field that is not there
trigger:
  object: Invoice
condition: 'record.no_such_field == "x"'
actions:
  - type: field_update
    set:
      followup: true
`)
			publish(t, db, tenant, "Invoice", "x", "user-1")

			fired, err := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 0 {
				t.Fatalf("fired %d times on an unevaluable condition, want 0", fired)
			}
			if got := objects.seen(); len(got) != 0 {
				t.Fatalf("an unevaluable condition still wrote: %+v", got)
			}
		})
	}
}

// A disabled automation is kept and skipped. Deleting one to pause it loses the
// definition, which is why `enabled:` exists at all.
func TestDisabledAutomationDoesNotFire(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			objects.records["Invoice/x"] = map[string]any{"status": "posted"}

			saveAutomation(t, db, tenant, `
id: paused
name: Paused
enabled: false
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      followup: true
`)
			publish(t, db, tenant, "Invoice", "x", "user-1")

			fired, err := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 0 {
				t.Fatalf("a disabled automation fired %d times", fired)
			}
			// …and it is still there to re-enable.
			if _, err := Get(ctx, db, tenant, "paused"); err != nil {
				t.Fatalf("a disabled automation was lost: %v", err)
			}
		})
	}
}

// An automation created today does not replay the tenant's history: the cursor
// starts at the feed's high-water mark, written at save rather than lazily on
// the first pass (the window WP-3.1b found).
func TestNewAutomationDoesNotReplayHistory(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			for i := 0; i < 5; i++ {
				id := fmt.Sprintf("old-%d", i)
				objects.records["Invoice/"+id] = map[string]any{"status": "posted"}
				publish(t, db, tenant, "Invoice", id, "user-1")
			}

			saveAutomation(t, db, tenant, `
id: fresh
name: Created after the fact
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      followup: true
`)
			fired, err := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 0 {
				t.Fatalf("a new automation replayed %d historical changes", fired)
			}

			// Non-vacuity: a change *after* the save does fire it.
			objects.records["Invoice/new"] = map[string]any{"status": "posted"}
			publish(t, db, tenant, "Invoice", "new", "user-1")
			fired, err = NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 1 {
				t.Fatalf("fired %d times on a change after the save, want 1", fired)
			}
		})
	}
}

// An automation only sees the object it subscribes to.
func TestAutomationIgnoresOtherObjects(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			objects := newFakeObjects()
			objects.records["Contact/c"] = map[string]any{"status": "posted"}
			objects.records["Invoice/i"] = map[string]any{"status": "posted"}

			saveAutomation(t, db, tenant, `
id: invoices-only
name: Invoices only
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      followup: true
`)
			publish(t, db, tenant, "Contact", "c", "user-1")
			fired, err := NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 0 {
				t.Fatalf("an Invoice automation fired on a Contact change")
			}

			publish(t, db, tenant, "Invoice", "i", "user-1")
			fired, err = NewRunner(db, objects, &fakePlugins{}, nil, outbound.Policy{}).RunOnce(ctx, tenant)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if fired != 1 {
				t.Fatalf("fired %d times on its own object, want 1", fired)
			}
		})
	}
}

// A failed action is recorded as failed, and does not stop the tenant's other
// automations from running.
func TestFailedActionIsRecordedAndDoesNotStopThePass(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			broken := newFakeObjects()
			broken.records["Invoice/x"] = map[string]any{"status": "posted"}
			broken.err = fmt.Errorf("storage said no")

			saveAutomation(t, db, tenant, `
id: breaks
name: Breaks
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      followup: true
`)
			plugins := &fakePlugins{}
			saveAutomation(t, db, tenant, `
id: works
name: Works
trigger:
  object: Invoice
actions:
  - type: call_plugin
    plugin: com.acme.x
    fn: run
`)
			publish(t, db, tenant, "Invoice", "x", "user-1")

			if _, err := NewRunner(db, broken, plugins, nil, outbound.Policy{}).RunOnce(ctx, tenant); err != nil {
				t.Fatalf("RunOnce returned a pass-level error for one automation's failure: %v", err)
			}
			runs, err := Runs(ctx, db, tenant, "breaks", 0)
			if err != nil {
				t.Fatalf("Runs: %v", err)
			}
			if len(runs) != 1 || runs[0].Outcome != OutcomeFailed {
				t.Fatalf("runs for the broken automation = %+v, want one failed", runs)
			}
			if !strings.Contains(runs[0].Detail, "storage said no") {
				t.Fatalf("the failure detail lost the cause: %q", runs[0].Detail)
			}
			// The other automation still ran.
			if got := plugins.seen(); len(got) != 1 || got[0] != "com.acme.x.run" {
				t.Fatalf("the working automation did not run: %v", got)
			}
		})
	}
}

// publish appends a change-feed entry as if CRUD had written the record.
func publish(t *testing.T, db *storage.DB, tenant tenancy.ID, object, refID, actor string) {
	t.Helper()
	if err := appendFeed(context.Background(), db, tenant, object, refID, actor); err != nil {
		t.Fatalf("publish %s/%s: %v", object, refID, err)
	}
}

// automationApprover is a user holding every grant the test definitions need.
// Save refuses an automation whose authority the creator does not hold
// (INV-T3), so the fixture has to be a real principal with real grants — which
// is itself worth having in the suite: it is the bound under test in
// TestSaveRefusesAuthorityTheCreatorLacks.
func automationApprover(t *testing.T, db *storage.DB, tenant tenancy.ID) authz.Actor {
	t.Helper()
	ctx := context.Background()
	user := identity.UserID(idgen.New())
	role, err := authz.CreateRole(ctx, db, tenant, "automation-approver-"+string(user), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, g := range [][2]string{
		{"Invoice", "read"}, {"Invoice", "update"},
		{"Contact", "read"}, {"Contact", "update"},
		{"plugin", "invoke"},
	} {
		if err := authz.GrantPermission(ctx, db, tenant, role, g[0], g[1], ""); err != nil {
			t.Fatalf("GrantPermission: %v", err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.Actor{TenantID: tenant, UserID: user}
}

// INV-T3 at the moment an automation's authority is created: it cannot hold a
// permission the person creating it does not. An automation is far easier to
// create than a plugin is to install, so without this bound "create an
// automation" is a privilege-escalation primitive for anyone who can reach the
// route.
func TestSaveRefusesAuthorityTheCreatorLacks(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			// A reader, who may not update anything.
			user := identity.UserID(idgen.New())
			role, err := authz.CreateRole(ctx, db, tenant, "reader", false)
			if err != nil {
				t.Fatalf("CreateRole: %v", err)
			}
			if err := authz.GrantPermission(ctx, db, tenant, role, "Invoice", "read", ""); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}
			if err := authz.AssignRole(ctx, db, tenant, user, role); err != nil {
				t.Fatalf("AssignRole: %v", err)
			}
			reader := authz.Actor{TenantID: tenant, UserID: user}

			src := "id: escalate\nname: Escalate\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {status: paid}}\n"
			_, err = Save(ctx, db, tenant, []byte(src), reader)
			if !errors.Is(err, ErrCapabilityNotHeld) {
				t.Fatalf("Save by a reader = %v, want ErrCapabilityNotHeld", err)
			}
			if !strings.Contains(err.Error(), "Invoice:update") {
				t.Fatalf("the refusal does not name the missing permission: %v", err)
			}
			// Nothing was left behind: no automation, and no role to inherit.
			if _, err := Get(ctx, db, tenant, "escalate"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a refused save stored the automation anyway: %v", err)
			}
			if _, err := authz.RoleByName(ctx, db, tenant, PrincipalFor("escalate")); !errors.Is(err, authz.ErrRoleNotFound) {
				t.Fatalf("a refused save left a role behind: %v", err)
			}

			// Non-vacuity: the same definition saved by someone who *does* hold
			// the grant succeeds, so the refusal is about authority and not
			// about the definition.
			if _, err := Save(ctx, db, tenant, []byte(src), automationApprover(t, db, tenant)); err != nil {
				t.Fatalf("Save by a fully-granted creator: %v", err)
			}
		})
	}
}

// Deleting an automation takes its authority with it. A role left behind is
// authority a later automation under the same id would silently inherit.
func TestDeleteRevokesAuthority(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			saveAutomation(t, db, tenant, `
id: temp
name: Temporary
trigger:
  object: Invoice
actions:
  - type: field_update
    set:
      status: paid
`)
			if _, err := authz.RoleByName(ctx, db, tenant, PrincipalFor("temp")); err != nil {
				t.Fatalf("precondition: the save did not create a role: %v", err)
			}
			if err := Delete(ctx, db, tenant, "temp"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := authz.RoleByName(ctx, db, tenant, PrincipalFor("temp")); !errors.Is(err, authz.ErrRoleNotFound) {
				t.Fatalf("delete left the automation's role behind: %v", err)
			}
		})
	}
}

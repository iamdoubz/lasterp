//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.3b's acceptance criterion: *a scheduled plugin job runs, retries and
// dead-letters like an async hook.* Carries **INV-X1** through the queue (a
// queued call has exactly the manifest's capabilities and no more), **INV-T4**
// (it runs as the plugin's own principal, never a user's) and INV-S4's
// no-silent-drops rule in the shape WP-3.1b gave it.
//
// The corpus is WP-3.1b's `hooks` module, reused rather than extended: `note`
// records what it was called with in plugin-scoped kv — so a test can see the
// plugin really executed rather than infer it from a job row — and `boom` fails
// on purpose, which is what the retry and dead-letter halves need.

// scheduledManifest declares a cron whose firing calls `first`.
//
// The object capabilities are not decoration: a WASM module whose imports are
// not all satisfied cannot be instantiated at all, and the `hooks` corpus
// member imports the object host functions for its `spawn` export. Without
// them every scheduled run fails with an instantiation error — INV-X1 working
// exactly as designed, and a very effective way to hide the failure the test is
// actually about.
func scheduledManifest(cron, first string) string {
	return `
id: com.acme.hooks
version: 1.0.0
functions: [` + first + `]
capabilities:
  schedule: ["` + cron + `"]
  objects:
    - {type: Widget, access: read}
    - {type: Widget, access: write}
`
}

// scheduleApprover holds every grant scheduledManifest requests.
func scheduleApprover(t *testing.T, db *storage.DB, tenant tenancy.ID) authz.Actor {
	t.Helper()
	return approver(t, db, tenant,
		[2]string{"Widget", "read"}, [2]string{"Widget", "create"}, [2]string{"Widget", "update"})
}

func jobsHost(db *storage.DB) Host {
	return Host{DB: db, Limits: DefaultLimits}
}

// runQueue drains the tenant's queue with the plugin handler wired in.
func runQueue(t *testing.T, db *storage.DB, tenant tenancy.ID, now time.Time) int {
	t.Helper()
	reg := jobs.NewRegistry()
	reg.Register(JobKind, JobHandler(jobsHost(db)))
	ran, err := jobs.NewRunner(db, reg, "test").RunOnce(context.Background(), tenant, now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return ran
}

// TestScheduledPluginJobRuns is the AC's happy path, end to end: a manifest
// declares a cron, the install creates the schedule, the tick enqueues the job,
// and the runner actually invokes the plugin.
//
// The proof that it ran is the plugin's *own* kv write, not the job row: a job
// marked done only says the handler returned nil, which a handler that quietly
// did nothing also does.
func TestScheduledPluginJobRuns(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			admin := scheduleApprover(t, db, tenant)

			p, err := Install(ctx, db, tenant, []byte(scheduledManifest("0 2 * * *", "note")),
				corpusModule(t, "hooks"), Customizations{}, admin)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}

			// The install created the schedule, owned by the plugin.
			list, err := jobs.ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("install created %d schedules, want 1", len(list))
			}
			if list[0].Owner != p.Principal() {
				t.Fatalf("schedule owner = %q, want the plugin principal %q", list[0].Owner, p.Principal())
			}

			// Time is driven from the firing the install actually stored, not
			// from a fixed date: Install computes the next occurrence from the
			// real clock, so a hardcoded instant is in its past and the schedule
			// silently never fires.
			fire := list[0].NextRunAt

			// Not due yet: nothing runs.
			if ran := runQueue(t, db, tenant, fire.Add(-time.Minute)); ran != 0 {
				t.Fatalf("ran %d jobs before the schedule was due", ran)
			}

			// Due: the job is enqueued and the plugin is invoked.
			if ran := runQueue(t, db, tenant, fire); ran != 1 {
				t.Fatalf("ran %d jobs at the firing time, want 1", ran)
			}

			// The plugin really executed — its own kv write says so.
			value, found, err := kvGet(ctx, db, tenant, p.ID, "seen::")
			if err != nil {
				t.Fatalf("kvGet: %v", err)
			}
			if !found || value == "" {
				t.Fatal("the scheduled job reported success but the plugin never ran")
			}

			// Nothing failed on the way.
			letters, err := jobs.DeadLetters(ctx, db, tenant, "")
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 0 {
				t.Fatalf("a successful run filed dead letters: %+v", letters)
			}
		})
	}
}

// The other half of the AC: a scheduled job that fails retries on the queue's
// backoff and is then filed where a person can see it, naming the plugin —
// exactly what WP-3.1b promised for an async hook delivery, so a plugin author
// tuning around one failure story does not discover a second.
func TestScheduledPluginJobRetriesAndDeadLetters(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			admin := scheduleApprover(t, db, tenant)
			if _, err := Install(ctx, db, tenant, []byte(scheduledManifest("0 2 * * *", "boom")),
				corpusModule(t, "hooks"), Customizations{}, admin); err != nil {
				t.Fatalf("Install: %v", err)
			}

			list, err := jobs.ListSchedules(ctx, db, tenant)
			if err != nil || len(list) != 1 {
				t.Fatalf("precondition: schedules = %v, err = %v", list, err)
			}
			fire := list[0].NextRunAt

			// The passes stay inside the same day, so the daily schedule fires
			// exactly once and every later pass is a *retry* of that one job
			// rather than a fresh firing — which is what makes the dead-letter
			// count below mean what it says. The spacing clears the backoff.
			for attempt := 0; attempt < jobs.MaxAttempts; attempt++ {
				runQueue(t, db, tenant, fire.Add(time.Duration(attempt)*10*time.Minute))
			}

			letters, err := jobs.DeadLetters(ctx, db, tenant, JobKind)
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 {
				t.Fatalf("got %d dead letters after %d failed attempts, want 1", len(letters), jobs.MaxAttempts)
			}
			if letters[0].Attempts != jobs.MaxAttempts {
				t.Fatalf("dead letter records %d attempts, want %d — it gave up early or late",
					letters[0].Attempts, jobs.MaxAttempts)
			}
			// The operator needs to know *which* plugin, from the letter alone.
			if !strings.Contains(letters[0].Payload, "com.acme.hooks") {
				t.Fatalf("dead letter does not name the plugin: %+v", letters[0])
			}
			if !strings.Contains(letters[0].Error, "deliberate failure") {
				t.Fatalf("dead letter lost the plugin's own cause: %q", letters[0].Error)
			}

			// And the queue moved on rather than stalling on the failed head.
			if ran := runQueue(t, db, tenant, fire.Add(24*time.Hour)); ran != 1 {
				// One: the *next* day's firing. A stalled queue would replay the
				// dead job instead.
				t.Fatalf("after the dead letter the queue ran %d jobs, want the next firing only", ran)
			}
		})
	}
}

// Uninstalling takes the schedules with it, for the reason it takes the role:
// a cron row left behind is work that keeps firing for code that is gone, and
// it would fire under a reinstalled *different* plugin's id.
func TestUninstallRemovesSchedules(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			admin := scheduleApprover(t, db, tenant)
			now := time.Now().UTC()

			if _, err := Install(ctx, db, tenant, []byte(scheduledManifest("0 2 * * *", "note")),
				corpusModule(t, "hooks"), Customizations{}, admin); err != nil {
				t.Fatalf("Install: %v", err)
			}
			list, err := jobs.ListSchedules(ctx, db, tenant)
			if err != nil || len(list) != 1 {
				t.Fatalf("precondition: schedules = %v, err = %v", list, err)
			}

			if err := Uninstall(ctx, db, tenant, "com.acme.hooks", "admin"); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			list, err = jobs.ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 0 {
				t.Fatalf("uninstall left %d schedules behind: %+v", len(list), list)
			}
			// And a tick after the uninstall enqueues nothing.
			n, err := jobs.TickSchedules(ctx, db, tenant, now.AddDate(0, 0, 1))
			if err != nil {
				t.Fatalf("TickSchedules: %v", err)
			}
			if n != 0 {
				t.Fatalf("an uninstalled plugin's schedule still fired %d jobs", n)
			}
		})
	}
}

// INV-X1 through the queue: `enqueue_job` exists only for a plugin whose
// manifest asked for it. The primary enforcement is the host-function table —
// an ungranted import cannot be bound, so the module cannot instantiate — and
// this asserts the second gate, which catches a table built from the wrong
// manifest.
func TestEnqueueRefusedWithoutTheCapability(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)

			ungranted := &Installed{ID: "com.acme.x", Manifest: &Manifest{
				ID: "com.acme.x", Functions: []string{"run"},
			}}
			inv := &invocation{host: jobsHost(db), tenant: tenant, plugin: ungranted, budget: 10}
			if _, err := hostEnqueueJob(ctx, inv, map[string]any{"fn": "run"}); !errors.Is(err, ErrJobsNotGranted) {
				t.Fatalf("enqueue without the capability: err = %v, want ErrJobsNotGranted", err)
			}

			// Non-vacuity: the identical call succeeds once the manifest asks.
			granted := &Installed{ID: "com.acme.x", Manifest: &Manifest{
				ID: "com.acme.x", Functions: []string{"run"},
				Capabilities: Capabilities{Jobs: true},
			}}
			inv = &invocation{host: jobsHost(db), tenant: tenant, plugin: granted, budget: 10}
			if _, err := hostEnqueueJob(ctx, inv, map[string]any{"fn": "run"}); err != nil {
				t.Fatalf("enqueue with the capability: %v", err)
			}
		})
	}
}

// A plugin may queue **its own** declared functions and nothing else. Naming an
// undeclared export would make the manifest an administrator read an incomplete
// answer to "what can this run"; the plugin id is written by the host, never
// read from the request, so there is no shape in which one plugin queues
// another's work.
func TestEnqueueOnlyQueuesDeclaredOwnFunctions(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			p := &Installed{ID: "com.acme.x", Manifest: &Manifest{
				ID: "com.acme.x", Functions: []string{"run"},
				Capabilities: Capabilities{Jobs: true},
			}}
			inv := &invocation{host: jobsHost(db), tenant: tenant, plugin: p, budget: 100}

			if _, err := hostEnqueueJob(ctx, inv, map[string]any{"fn": "not_declared"}); err == nil {
				t.Fatal("queued an undeclared function")
			}

			// A request that tries to name a different plugin is ignored, not
			// honoured: the payload's plugin_id comes from the invocation.
			out, err := hostEnqueueJob(ctx, inv, map[string]any{
				"fn": "run", "plugin_id": "com.acme.victim",
			})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			id, _ := out.(map[string]any)["job_id"].(string)
			if id == "" {
				t.Fatal("enqueue returned no job id")
			}
			job, err := jobs.Claim(ctx, db, tenant, "t", time.Now().UTC().Add(time.Minute))
			if err != nil || job == nil {
				t.Fatalf("Claim: job = %v, err = %v", job, err)
			}
			var payload JobPayload
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.PluginID != "com.acme.x" {
				t.Fatalf("payload names plugin %q; a plugin must not be able to queue another's work",
					payload.PluginID)
			}
		})
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// JobKind is the queue kind every plugin invocation runs under. One kind, not
// one per plugin: the payload names the plugin, so a single handler covers the
// whole surface and `jobs` never learns what a plugin is.
const JobKind = "plugin.invoke"

// MaxPendingJobsPerPlugin bounds how much work one plugin may have queued.
//
// This is the amplification guard, and it is the reason `enqueue_job` is
// capability-gated at all. The function grants no *new* authority over data —
// a queued call runs with exactly the capabilities the manifest already
// declared — but it does let a plugin multiply itself: a hook that enqueues two
// jobs, each of which enqueues two more, is a fork bomb with a database behind
// it. The cap turns that into a refusal the plugin can see.
const MaxPendingJobsPerPlugin = 1000

// ErrJobsNotGranted is returned when a plugin enqueues without the capability.
var ErrJobsNotGranted = errors.New("plugins: this plugin did not declare capabilities.jobs")

// ErrTooManyPendingJobs is returned when a plugin is at its queue allowance.
var ErrTooManyPendingJobs = errors.New("plugins: too many pending jobs for this plugin")

// JobPayload is what a queued plugin invocation carries.
//
// PluginID is written by the host from the invocation, never read from the
// plugin's request: a plugin may queue **its own** functions and nothing else.
// Letting the caller name the plugin would be lateral authority — plugin A
// scheduling plugin B's function — which is precisely what "no plugin can call
// another" (WP-3.2-decisions §4) rules out at every other surface.
type JobPayload struct {
	PluginID string          `json:"plugin_id"`
	Fn       string          `json:"fn"`
	Arg      json.RawMessage `json:"arg,omitempty"`
}

// hostEnqueueJob is the `enqueue_job` host function, deferred out of WP-3.1b
// for want of a runner and landed here with one.
func hostEnqueueJob(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	if !inv.plugin.Manifest.Capabilities.AllowsJobs() {
		// Belt to the host-function table's braces: a plugin without the
		// capability has no `lasterp_enqueue_job` to import and cannot
		// instantiate if it tries (INV-X1). This is the second gate, checked
		// anyway, because a table built from the wrong manifest would otherwise
		// be a silent escalation.
		return nil, ErrJobsNotGranted
	}

	fn, _ := req["fn"].(string)
	if !declaresFunction(inv.plugin.Manifest, fn) {
		// The callable surface is declared, not discovered — the same rule
		// hooks and endpoints follow. A plugin cannot queue an export it did
		// not list, so the manifest an administrator read is still the whole
		// answer to "what can this run".
		return nil, fmt.Errorf("plugins: %q is not a declared function", fn)
	}

	var arg json.RawMessage
	if raw, ok := req["arg"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("plugins: enqueue_job argument is not encodable: %w", err)
		}
		arg = encoded
	}
	payload, err := json.Marshal(JobPayload{PluginID: inv.plugin.ID, Fn: fn, Arg: arg})
	if err != nil {
		return nil, err
	}

	runAt := time.Now().UTC()
	if delay, ok := req["delay_seconds"].(float64); ok && delay > 0 {
		runAt = runAt.Add(time.Duration(delay) * time.Second)
	}

	// The plugin's dedupe key is namespaced by its id, so two plugins choosing
	// the same key do not collapse into one job.
	dedupe := ""
	if key, ok := req["dedupe_key"].(string); ok && key != "" {
		dedupe = inv.plugin.ID + ":" + key
	}

	pending, err := jobs.CountPending(ctx, inv.host.DB, inv.tenant, JobKind, inv.plugin.ID)
	if err != nil {
		return nil, err
	}
	if pending >= MaxPendingJobsPerPlugin {
		return nil, fmt.Errorf("%w: %d already queued", ErrTooManyPendingJobs, pending)
	}

	id, err := jobs.Enqueue(ctx, inv.host.DB, inv.tenant, JobKind, payload, runAt, dedupe)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job_id": id}, nil
}

func declaresFunction(m *Manifest, fn string) bool {
	for _, f := range m.Functions {
		if f == fn {
			return true
		}
	}
	return false
}

// JobHandler is the queue handler for JobKind: it loads the named plugin and
// calls the named function.
//
// The plugin runs as its own principal here exactly as it does in a hook or an
// endpoint (INV-T4) — a queued job has no triggering user to borrow authority
// from, which is the clearest case for why plugin authority never came from the
// caller in the first place.
//
// A plugin uninstalled between enqueue and execution makes the job fail and
// eventually dead-letter, naming the plugin. That is deliberate: silently
// dropping the work would leave an operator with a job that vanished.
func JobHandler(host Host) jobs.Handler {
	return func(ctx context.Context, tenant tenancy.ID, payload []byte) error {
		var p JobPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("plugins: job payload is not readable: %w", err)
		}
		installed, err := Get(ctx, host.DB, tenant, p.PluginID)
		if err != nil {
			return fmt.Errorf("plugins: job for %s: %w", p.PluginID, err)
		}
		if !declaresFunction(installed.Manifest, p.Fn) {
			// The manifest changed under a queued job — an upgrade that dropped
			// the function. Refused rather than called: the surface the running
			// manifest declares is the only one that may execute.
			return fmt.Errorf("plugins: %s no longer declares %q", p.PluginID, p.Fn)
		}
		arg := []byte(p.Arg)
		if len(arg) == 0 {
			arg = []byte("{}")
		}
		jobHost := host
		jobHost.Limits.Timeout = jobs.LeaseDuration / 2
		_, err = Call(ctx, jobHost, tenant, installed, p.Fn, arg)
		return err
	}
}

// ScheduleID is the job_schedules row id for one of a plugin's cron entries.
// Deterministic, so reinstalling the same plugin replaces its schedules rather
// than accumulating them.
func ScheduleID(pluginID string, i int) string {
	return fmt.Sprintf("plugin:%s:%d", pluginID, i)
}

// SyncSchedules makes the tenant's job_schedules match the manifest: one row
// per declared cron expression, owned by the plugin's principal.
//
// Called at install. The first declared function is the one a schedule fires —
// docs/05's `schedule:` is a list of times, not of targets, and inventing a
// second syntax to pair them was not worth it while no plugin needs more than
// one scheduled entry point (see docs/notes/WP-3.3-decisions.md §7).
func SyncSchedules(ctx context.Context, host Host, tenant tenancy.ID, p *Installed, now time.Time) error {
	owner := p.Principal()
	if err := jobs.DeleteSchedulesByOwner(ctx, host.DB, tenant, owner); err != nil {
		return err
	}
	if len(p.Manifest.Capabilities.Schedule) == 0 {
		return nil
	}
	fn := p.Manifest.Functions[0]
	for i, expr := range p.Manifest.Capabilities.Schedule {
		payload, err := json.Marshal(JobPayload{PluginID: p.ID, Fn: fn})
		if err != nil {
			return err
		}
		if err := jobs.UpsertSchedule(ctx, host.DB, tenant, jobs.Schedule{
			ID:      ScheduleID(p.ID, i),
			Kind:    JobKind,
			Cron:    expr,
			Payload: payload,
			Owner:   owner,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

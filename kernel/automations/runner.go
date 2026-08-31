// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/expr"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// FeedBatch is how many feed entries one pass reads per automation.
const FeedBatch = 100

// JobKind is the queue kind a scheduled automation runs under.
const JobKind = "automation.run"

// Objects is the record access an automation's actions need.
//
// Declared here, in the package that consumes it, and implemented by the
// composition root — the same shape metadata.Hooks uses, and for the same
// reason: this package must not reach into CRUD construction, and a test must
// be able to supply its own.
//
// **Both methods must go through the ordinary authorization gate and audit.**
// An implementation that writes behind CRUD would make an automation a way to
// bypass the pipeline, which is INV-T2 and the whole reason ADR-014 puts
// autonomy inside a fence.
type Objects interface {
	// Get reads one record. A missing record is (nil, nil): the row may have
	// been deleted between the feed entry and this pass, which is ordinary.
	Get(ctx context.Context, tenant tenancy.ID, object, id string) (map[string]any, error)
	// Update applies changes to one record as the given actor.
	Update(ctx context.Context, tenant tenancy.ID, actor authz.Actor, object, id string, changes map[string]any) error
}

// Plugins is the plugin invocation an automation's call_plugin action needs.
// Declared at the consumer side so this package does not import kernel/plugins.
type Plugins interface {
	Enqueue(ctx context.Context, tenant tenancy.ID, plugin, fn string, arg []byte) error
}

// Runner fires a tenant's automations.
type Runner struct {
	db      *storage.DB
	objects Objects
	plugins Plugins
}

func NewRunner(db *storage.DB, objects Objects, plugins Plugins) *Runner {
	return &Runner{db: db, objects: objects, plugins: plugins}
}

// RunOnce delivers one pass of feed-triggered automations for a tenant and
// reports how many fired their actions.
//
// A failure in one automation is recorded and the pass continues: automations
// are independent of each other, and one broken rule must not stop the rest of
// a tenant's from running.
func (r *Runner) RunOnce(ctx context.Context, tenant tenancy.ID) (int, error) {
	list, err := List(ctx, r.db, tenant, true)
	if err != nil {
		return 0, err
	}
	fired := 0
	for i := range list {
		d := list[i].Definition
		if d.TriggerKind() != TriggerObject {
			continue // schedule triggers arrive through the job queue
		}
		n, err := r.deliver(ctx, tenant, d)
		fired += n
		if err != nil {
			return fired, err
		}
	}
	return fired, nil
}

func (r *Runner) deliver(ctx context.Context, tenant tenancy.ID, d *Definition) (int, error) {
	cursor, err := readCursor(ctx, r.db, tenant, d.ID)
	if err != nil {
		return 0, err
	}
	changes, err := changefeed.Read(ctx, r.db, tenant, cursor, FeedBatch, nil)
	if err != nil {
		return 0, err
	}

	fired := 0
	for _, c := range changes {
		// An automation does not react to its own writes. Without this a
		// field_update on the object it subscribes to loops forever — the same
		// hazard WP-3.1b's self-suppression exists for, and the reason
		// change_feed carries an actor at all.
		if c.Object != d.Trigger.Object || c.ActorID == d.Principal() {
			if err := advanceCursor(ctx, r.db, tenant, d.ID, cursor, c.Cursor); err != nil {
				return fired, err
			}
			cursor = c.Cursor
			continue
		}

		matched, err := r.fire(ctx, tenant, d, c)
		if err != nil {
			return fired, err
		}
		if matched {
			fired++
		}
		if err := advanceCursor(ctx, r.db, tenant, d.ID, cursor, c.Cursor); err != nil {
			return fired, err
		}
		cursor = c.Cursor
	}
	return fired, nil
}

// fire evaluates one automation against one change and runs its actions.
func (r *Runner) fire(ctx context.Context, tenant tenancy.ID, d *Definition, c changefeed.Change) (bool, error) {
	ref := fmt.Sprintf("%s:%d", c.RefID, c.Cursor)

	record, err := r.objects.Get(ctx, tenant, d.Trigger.Object, c.RefID)
	if err != nil {
		return false, recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeFailed, "read record: "+err.Error())
	}
	if record == nil {
		// Deleted between the feed entry and this pass. Not a failure: the feed
		// carries no verb, so "gone" is an ordinary outcome rather than an
		// error to raise at anyone.
		return false, recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeSkipped, "record no longer exists")
	}

	if !r.matches(d, record) {
		return false, recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeSkipped, "condition did not match")
	}

	if err := r.act(ctx, tenant, d, c.RefID, record); err != nil {
		return false, recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeFailed, err.Error())
	}
	return true, recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeMatched, "")
}

// matches evaluates the condition, fail-closed.
//
// An empty condition matches everything — that is what "no condition" means.
// Anything else is ADR-022's rule verbatim: false, an evaluation error, a cost
// overrun, a non-boolean result and an expression that no longer compiles all
// mean *do not fire*. An automation that acts when its rule could not be
// evaluated is the failure mode worth engineering against, because the action
// is a write.
func (r *Runner) matches(d *Definition, record map[string]any) bool {
	if d.Condition == "" {
		return true
	}
	prg, err := expr.Get(d.Condition)
	if err != nil {
		return false
	}
	ok, err := prg.Eval(record, expr.Actor{ID: d.Principal(), Tenant: "", Roles: nil})
	return err == nil && ok
}

// act runs every action in order, stopping at the first failure.
func (r *Runner) act(ctx context.Context, tenant tenancy.ID, d *Definition, recordID string, record map[string]any) error {
	actor := authz.Actor{TenantID: tenant, UserID: identity.UserID(d.Principal())}
	for i := range d.Actions {
		a := &d.Actions[i]
		switch a.Type {
		case ActionFieldUpdate:
			changes := make(map[string]any, len(a.Set))
			for k, v := range a.Set {
				// An assignment that would not change anything is dropped, so
				// an automation that fires repeatedly does not write repeatedly
				// — every no-op write is a feed entry, and a feed entry is what
				// wakes every other automation in the tenant.
				if existing, ok := record[k]; !ok || !sameScalar(existing, v) {
					changes[k] = v
				}
			}
			if len(changes) == 0 {
				continue
			}
			if err := r.objects.Update(ctx, tenant, actor, d.Trigger.Object, recordID, changes); err != nil {
				return fmt.Errorf("action %d (field_update): %w", i, err)
			}
		case ActionCallPlugin:
			arg, err := json.Marshal(map[string]any{
				"automation": d.ID, "object": d.Trigger.Object, "record_id": recordID,
			})
			if err != nil {
				return err
			}
			if r.plugins == nil {
				return fmt.Errorf("action %d (call_plugin): no plugin host is wired", i)
			}
			if err := r.plugins.Enqueue(ctx, tenant, a.Plugin, a.Fn, arg); err != nil {
				return fmt.Errorf("action %d (call_plugin): %w", i, err)
			}
		default:
			// Unreachable: Validate refuses an unknown type at parse time. Kept
			// as a refusal rather than a silent skip, because a definition that
			// somehow reached here with an unknown action must not be reported
			// as having run.
			return fmt.Errorf("action %d: %q is not implemented", i, a.Type)
		}
	}
	return nil
}

// sameScalar compares a stored value with a declared literal well enough to
// tell "already set" from "needs setting".
//
// Deliberately shallow: YAML literals are scalars, and comparing them through
// their string form sidesteps the int64/float64/json.Number disagreement
// between the YAML decoder and the storage layer, which is the actual source of
// spurious rewrites here.
func sameScalar(a, b any) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// SchedulePayload is what a scheduled automation's queued job carries.
type SchedulePayload struct {
	AutomationID string `json:"automation_id"`
}

// SyncSchedules makes job_schedules match the tenant's scheduled automations.
func SyncSchedules(ctx context.Context, db *storage.DB, tenant tenancy.ID, now time.Time) error {
	list, err := List(ctx, db, tenant, true)
	if err != nil {
		return err
	}
	// Owned rows are replaced wholesale rather than diffed: the definition is
	// the source of truth and a stale row is work firing for a rule nobody
	// wrote any more.
	if err := jobs.DeleteSchedulesByOwner(ctx, db, tenant, scheduleOwner); err != nil {
		return err
	}
	for i := range list {
		d := list[i].Definition
		if d.TriggerKind() != TriggerSchedule {
			continue
		}
		payload, err := json.Marshal(SchedulePayload{AutomationID: d.ID})
		if err != nil {
			return err
		}
		if err := jobs.UpsertSchedule(ctx, db, tenant, jobs.Schedule{
			ID:      "automation:" + d.ID,
			Kind:    JobKind,
			Cron:    d.Trigger.Schedule,
			Payload: payload,
			Owner:   scheduleOwner,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

// scheduleOwner scopes the bulk delete in SyncSchedules. One owner for every
// automation rather than one per automation, because the sync replaces the
// whole set.
const scheduleOwner = "automations"

// JobHandler runs a scheduled automation's actions.
//
// A scheduled automation has no triggering record, so `field_update` is refused
// for one at parse time and only `call_plugin` can appear here. The condition
// is evaluated against an empty record, which means a condition reading
// `record.*` denies — fail-closed, and visible in the run log rather than
// silent.
func (r *Runner) JobHandler() jobs.Handler {
	return func(ctx context.Context, tenant tenancy.ID, payload []byte) error {
		var p SchedulePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("automations: job payload is not readable: %w", err)
		}
		stored, err := Get(ctx, r.db, tenant, p.AutomationID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Deleted between the firing and this run. Nothing to do, and
				// not a failure worth retrying.
				return nil
			}
			return err
		}
		if !stored.Enabled {
			return nil
		}
		d := stored.Definition
		ref := "schedule:" + time.Now().UTC().Format(time.RFC3339)
		if !r.matches(d, map[string]any{}) {
			return recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeSkipped, "condition did not match")
		}
		if err := r.act(ctx, tenant, d, "", map[string]any{}); err != nil {
			// Recorded *and* returned: the run log is the operator's view, and
			// the returned error is what makes the queue retry and eventually
			// dead-letter it.
			_ = recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeFailed, err.Error())
			return err
		}
		return recordRun(ctx, r.db, tenant, d.ID, ref, OutcomeMatched, "")
	}
}

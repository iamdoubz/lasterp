// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/automations"
	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/plugins"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Permission tuple for managing automations. An automation writes as its own
// principal and can be given a condition, so creating one is an authority
// grant: it is gated like the capability admin surface, not like a CRUD object.
const objectAutomation = "Automation"

// crudObjectsAdapter implements automations.Objects over the metadata CRUD
// engines this deployment serves.
//
// **Every read and write goes through CRUD**, which is the whole point: an
// automation's action passes the same authorization gate, the same validation
// and the same audit as a human's write (INV-T2/T4/T5). An adapter that
// reached the tables directly would make an automation the side door around
// the pipeline, which is exactly what ADR-014's fence forbids.
type crudObjectsAdapter struct {
	db    *storage.DB
	cruds map[string]*metadata.CRUD
}

func (a crudObjectsAdapter) Get(ctx context.Context, tenant tenancy.ID, object, id string) (map[string]any, error) {
	crud, ok := a.cruds[object]
	if !ok {
		return nil, fmt.Errorf("app: no CRUD engine for %s", object)
	}
	// The actor is already bound by the runner (kernel/automations: fire binds
	// the automation principal before the read, precisely because this Get
	// authorizes). Not defaulted here: an adapter that invented an actor when
	// the context had none would be a way to read without one.
	rec, err := crud.Get(ctx, a.db, tenant, id)
	if err != nil {
		if errors.Is(err, metadata.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return rec, nil
}

func (a crudObjectsAdapter) Update(ctx context.Context, tenant tenancy.ID, actor authz.Actor, object, id string, changes map[string]any) error {
	crud, ok := a.cruds[object]
	if !ok {
		return fmt.Errorf("app: no CRUD engine for %s", object)
	}
	_, err := crud.Update(authz.WithActor(ctx, actor), a.db, tenant, id, changes)
	return err
}

// pluginEnqueueAdapter implements automations.Plugins by putting the call on
// the job queue rather than running it inline.
//
// Queued, not called: an automation runs on a background sweep, and a plugin
// invocation there would put a WASM module's wall-clock budget inside the loop
// that delivers every other automation in the tenant. The queue already has
// retries, dead letters and a lease — reusing it is why WP-3.3b built the
// runner before the automations that need it.
type pluginEnqueueAdapter struct{ db *storage.DB }

func (p pluginEnqueueAdapter) Enqueue(ctx context.Context, tenant tenancy.ID, plugin, fn string, arg []byte) error {
	payload, err := json.Marshal(plugins.JobPayload{PluginID: plugin, Fn: fn, Arg: arg})
	if err != nil {
		return err
	}
	_, err = jobs.Enqueue(ctx, p.db, tenant, plugins.JobKind, payload, time.Now().UTC(), "")
	return err
}

// StartAutomationRunner runs the job queue and the automation feed sweep until
// ctx is cancelled, returning a function that waits for both to stop.
//
// One ticker drives both because they are one story: a scheduled automation
// becomes a queued job, and a queued job may be a plugin call an automation
// asked for. Splitting them would mean two sweeps of the same tenant list at
// two cadences, with the scheduled half arriving on whichever fired first.
func StartAutomationRunner(ctx context.Context, db *storage.DB, host plugins.Host, every time.Duration) func() {
	if every <= 0 {
		every = 2 * time.Second
	}
	registry := jobs.NewRegistry()
	registry.Register(plugins.JobKind, plugins.JobHandler(host))

	runner := automations.NewRunner(db,
		// The plugin host already holds the CRUD engine per object, and it is a
		// superset of the gateway's: it includes the module-owned documents a
		// plugin may read but not write. An automation gets the same view, and
		// the read-only ones refuse its writes at the same gate they refuse a
		// plugin's.
		crudObjectsAdapter{db: db, cruds: host.Objects},
		pluginEnqueueAdapter{db: db},
		// A webhook needs the vault (the destination's URL) and the
		// deployment's outbound posture. Both already hang off the plugin
		// host, which is the composition root's one place for them — a second
		// copy is a second answer to "may this deployment reach its own
		// network".
		host.Keys, host.HTTP)
	registry.Register(automations.JobKind, runner.JobHandler())
	registry.Register(automations.WebhookJobKind, runner.WebhookHandler())

	onError := func(err error) { log.Printf("automation runner: %v", err) }
	stopJobs := jobs.Start(ctx, db, registry, "lasterp", every, onError)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tenants, err := listTenants(ctx, db)
				if err != nil {
					if ctx.Err() == nil {
						onError(fmt.Errorf("list tenants: %w", err))
					}
					continue
				}
				for _, tenant := range tenants {
					// Schedules are re-synced each pass rather than only on
					// write: an automation edited on another node must start
					// firing here too, and the sync is idempotent.
					if err := automations.SyncSchedules(ctx, db, tenant, time.Now()); err != nil && ctx.Err() == nil {
						onError(fmt.Errorf("sync schedules for %s: %w", tenant, err))
					}
					if _, err := runner.RunOnce(ctx, tenant); err != nil && ctx.Err() == nil {
						// One tenant's failure must not stop the others'. The
						// per-automation failures are already in automation_runs.
						onError(fmt.Errorf("run automations for %s: %w", tenant, err))
					}
				}
			}
		}
	}()
	return func() {
		<-done
		stopJobs()
	}
}

// --- HTTP surface ---
//
// Every capability is reachable via API (CLAUDE.md), so automations get real
// routes rather than living only in the runner. They are hand-registered
// actions rather than generic CRUD for the reason Invoice is: an automation is
// not a business record, its definition is a document that must be validated
// before it is stored, and a generic PATCH would let half a definition land.

func automationActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/automations", Object: objectAutomation,
			Summary: "List this tenant's automations", Handler: listAutomations(db)},
		{Method: "POST", Path: "/api/v1/automations", Object: objectAutomation, Write: true,
			Summary: "Create or replace an automation", Handler: saveAutomation(db)},
		{Method: "GET", Path: "/api/v1/automations/{id}", Object: objectAutomation,
			Summary: "Get one automation", Handler: getAutomation(db)},
		{Method: "DELETE", Path: "/api/v1/automations/{id}", Object: objectAutomation, Write: true,
			Summary: "Delete an automation", Handler: deleteAutomation(db)},
		{Method: "GET", Path: "/api/v1/automations/{id}/runs", Object: objectAutomation,
			Summary: "List an automation's recent runs", Handler: automationRuns(db)},
	}
}

func listAutomations(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectAutomation, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		list, err := automations.List(r.Context(), db, actor.TenantID, false)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for i := range list {
			out = append(out, automationJSON(&list[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"automations": out})
	}
}

func saveAutomation(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectAutomation, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "unreadable body", err.Error(), r.URL.Path)
			return
		}
		d, err := automations.Save(r.Context(), db, actor.TenantID, body, actor)
		if err != nil {
			// A malformed definition is the caller's mistake, not a fault: 422,
			// with the parser's own message, which names the field.
			writeProblem(w, http.StatusUnprocessableEntity, "invalid automation", err.Error(), r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "name": d.Name,
			"trigger": d.TriggerKind(), "enabled": d.IsEnabled()})
	}
}

func getAutomation(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectAutomation, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		stored, err := automations.Get(r.Context(), db, actor.TenantID, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, automations.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "unknown automation",
					"this tenant has no automation by that id", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, automationJSON(stored))
	}
}

func deleteAutomation(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectAutomation, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		id := r.PathValue("id")
		if err := automations.Delete(r.Context(), db, actor.TenantID, id); err != nil {
			fail(w, r, err)
			return
		}
		// The schedule goes with it on the next sweep; doing it here too means
		// a deleted automation stops firing immediately rather than within a
		// tick.
		if err := automations.SyncSchedules(r.Context(), db, actor.TenantID, time.Now()); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
	}
}

func automationRuns(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectAutomation, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		runs, err := automations.Runs(r.Context(), db, actor.TenantID, r.PathValue("id"), 0)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
}

func automationJSON(s *automations.Stored) map[string]any {
	return map[string]any{
		"id":         s.Definition.ID,
		"name":       s.Definition.Name,
		"trigger":    s.Definition.TriggerKind(),
		"object":     s.Definition.Trigger.Object,
		"schedule":   s.Definition.Trigger.Schedule,
		"enabled":    s.Enabled,
		"definition": s.YAML,
		"created_at": s.CreatedAt,
		"created_by": s.CreatedBy,
	}
}

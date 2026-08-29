// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/plugins"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// objectPlugin guards the plugin surface. Two actions, because they are two
// different powers: `manage` installs untrusted code and approves what it may
// touch, `invoke` merely runs something already approved.
const (
	objectPlugin = "plugin"
	actionInvoke = "invoke"
)

// pluginActions is the WP-3.1a management and invocation surface (ADR-007,
// docs/05).
//
// Installing is deliberately an authenticated API rather than a file drop:
// "everything is an API" applies, and an install is an approval decision that
// has to be attributable to a person (INV-T4). Bundle signing and registries
// are WP-3.2 — this installs a module a human already has.
func pluginActions(db *storage.DB, dispatcher *plugins.Dispatcher) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/plugins", Object: objectPlugin,
			Summary: "List installed plugins and the authority each was granted",
			Handler: listPlugins(db, dispatcher)},
		{Method: "POST", Path: "/api/v1/plugins", Object: objectPlugin, Write: true,
			Summary: "Install a plugin, approving the capabilities its manifest requests",
			Handler: installPlugin(db, dispatcher)},
		{Method: "DELETE", Path: "/api/v1/plugins/{id}", Object: objectPlugin, Write: true,
			Summary: "Uninstall a plugin and revoke the authority it was granted",
			Handler: uninstallPlugin(db, dispatcher)},
		{Method: "POST", Path: "/api/v1/plugins/{id}/call/{fn}", Object: objectPlugin, Write: true,
			Summary: "Call a function the plugin's manifest declares",
			Handler: callPlugin(db, dispatcher.Host())},
		{Method: "GET", Path: "/api/v1/plugins/{id}/dead-letters", Object: objectPlugin,
			Summary: "List deliveries this plugin failed every retry on",
			Handler: pluginDeadLetters(db)},
		{Method: "POST", Path: "/api/v1/plugins/{id}/reset-breaker", Object: objectPlugin, Write: true,
			Summary: "Close a tripped circuit breaker without waiting out the cooldown",
			Handler: resetPluginBreaker(db, dispatcher)},

		// The plugin-declared routes (WP-3.2a). **One pattern per method, not
		// one per plugin**: the set of live endpoints varies by tenant and by
		// install, and a mux mutated at runtime is a route table that cannot be
		// enumerated — which is how this codebase proves what it exposes
		// (routes_integrity_test.go). Resolution happens inside the handler,
		// where the tenant is known.
		{Method: "GET", Path: "/ext/{plugin}/{path...}", Object: objectPlugin,
			Summary: "Call a route a plugin declares in its manifest",
			Handler: extEndpoint(db, dispatcher.Host())},
		{Method: "POST", Path: "/ext/{plugin}/{path...}", Object: objectPlugin, Write: true,
			Summary: "Call a route a plugin declares in its manifest",
			Handler: extEndpoint(db, dispatcher.Host())},
	}
}

// extEndpoint serves `/ext/<plugin>/<path>` (ADR-007, docs/05).
//
// The gate is the caller's session plus `plugin:invoke` — the same permission
// as calling a function directly, because it is the same power: running plugin
// code. What that code may *do* is its own principal's business, decided at
// install; the caller's grants add nothing to it, which is what keeps an ext
// endpoint from being a way to launder authority in either direction.
func extEndpoint(db *storage.DB, host plugins.Host) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectPlugin, actionInvoke)
		if err != nil {
			fail(w, r, err)
			return
		}
		p, err := plugins.Get(r.Context(), db, tenant, r.PathValue("plugin"))
		if err != nil {
			if errors.Is(err, plugins.ErrNotFound) {
				// Same answer as an undeclared path below: whether a tenant has
				// a plugin by that id is not something a caller who may not use
				// it needs to learn.
				writeProblem(w, http.StatusNotFound, "not found",
					"no plugin serves that route", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, plugins.MaxEndpointRequestBytes+1))
		if err != nil || len(body) > plugins.MaxEndpointRequestBytes {
			writeProblem(w, http.StatusRequestEntityTooLarge, "request too large",
				"a plugin endpoint accepts at most 1MB", r.URL.Path)
			return
		}
		res, err := plugins.ServeEndpoint(r.Context(), host, tenant, p, plugins.EndpointRequest{
			Method: r.Method,
			Path:   "/" + strings.TrimPrefix(r.PathValue("path"), "/"),
			Query:  r.URL.RawQuery,
			Body:   string(body),
			Caller: string(actor.UserID),
		})
		if err != nil {
			if errors.Is(err, plugins.ErrNoSuchEndpoint) {
				writeProblem(w, http.StatusNotFound, "not found",
					"no plugin serves that route", r.URL.Path)
				return
			}
			// A plugin that traps or times out is the plugin failing, not the
			// server: 422 with the reason, as callPlugin already does.
			writeProblem(w, http.StatusUnprocessableEntity, "plugin call failed", err.Error(), r.URL.Path)
			return
		}
		// The content type is the clamped one — the allowlist in kernel/plugins
		// decides what a plugin's bytes may claim to be, and nosniff (set for
		// every response by withSecurityHeaders) stops the browser guessing
		// otherwise. ponytail: a *replayed* write returns the gateway's stored
		// response, which is always application/json; a plugin serving text/csv
		// from a POST endpoint sees that only on an idempotency replay.
		w.Header().Set("Content-Type", res.ContentType)
		w.WriteHeader(res.Status)
		if len(res.Body) > 0 {
			_, _ = w.Write(res.Body)
		}
	}
}

func listPlugins(db *storage.DB, dispatcher *plugins.Dispatcher) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectPlugin, actionManage); err != nil {
			fail(w, r, err)
			return
		}
		installed, err := plugins.List(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(installed))
		for _, p := range installed {
			granted := make([]string, 0, len(p.Granted))
			for _, g := range p.Granted {
				granted = append(granted, g[0]+":"+g[1])
			}
			out = append(out, map[string]any{
				"id": p.ID, "version": p.Version,
				// The approval record: what this tenant agreed the plugin may
				// do, in the same vocabulary as every other permission.
				"granted": granted,
				// The bytes' identity, which is what WP-3.2's signature check
				// will attach to.
				"sha256":       p.SHA256,
				"installed_at": p.InstalledAt,
				"installed_by": p.InstalledBy,
				// Breaker and cost, so an administrator can answer "is this
				// plugin healthy, and what is it costing me" without reading a
				// log (WP-3.1b-decisions.md §7). The latency figures are this
				// process's, since the restart said so.
				// What it exposes and where it calls out — the two questions
				// WP-3.2a's surfaces create, answered from the same place as
				// "what was it granted".
				"endpoints":         p.DeclaredEndpoints(),
				"outbound_hosts":    p.OutboundHosts(),
				"hook_failures":     p.HookFailures,
				"breaker_open":      p.BreakerOpen(),
				"breaker_opened_at": nullableTime(p.BreakerOpenedAt),
				"hooks":             dispatcher.Stats().For(p.ID),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// installPlugin approves and stores a plugin. One call decides both halves:
// Authorize is the permission (INV-T2) and the actor it returns is both the
// attribution (INV-T4) and the ceiling — kernel/plugins refuses any capability
// this actor does not itself hold (INV-T3).
func installPlugin(db *storage.DB, dispatcher *plugins.Dispatcher) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectPlugin, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		var body struct {
			Manifest string `json:"manifest"`
			Module   string `json:"module"` // base64
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		module, err := base64.StdEncoding.DecodeString(body.Module)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid module",
				"module must be base64-encoded WebAssembly", r.URL.Path)
			return
		}
		p, err := plugins.Install(r.Context(), db, tenant, []byte(body.Manifest), module, actor)
		if err != nil {
			if errors.Is(err, plugins.ErrCapabilityNotHeld) {
				// 403, not 422: this is an authorization answer. The
				// administrator is told exactly which capability they lack
				// rather than getting a plugin quietly installed with less
				// authority than its manifest claims.
				writeProblem(w, http.StatusForbidden, "capability not held", err.Error(), r.URL.Path)
				return
			}
			writeProblem(w, http.StatusUnprocessableEntity, "plugin refused", err.Error(), r.URL.Path)
			return
		}
		// A newly installed hook must fire on the very next write, not at the
		// cache TTL.
		dispatcher.Forget(tenant)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": p.ID, "version": p.Version, "sha256": p.SHA256, "status": "installed",
			// What the administrator is agreeing to, in plain language, at the
			// moment they agree to it. Outbound hosts and served routes are
			// part of that: neither is an authz permission the install could
			// check against the approver's grants (WP-3.2-decisions.md §1), so
			// showing them is the whole of the review.
			"warnings":       hookWarnings(p),
			"endpoints":      p.DeclaredEndpoints(),
			"outbound_hosts": p.OutboundHosts(),
		})
	}
}

func uninstallPlugin(db *storage.DB, dispatcher *plugins.Dispatcher) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectPlugin, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		id := r.PathValue("id")
		if err := plugins.Uninstall(r.Context(), db, tenant, id, string(actor.UserID)); err != nil {
			if errors.Is(err, plugins.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "unknown plugin",
					"this tenant has no plugin by that id", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		dispatcher.Forget(tenant)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "uninstalled"})
	}
}

// callPlugin runs one declared function. The caller's permission is `invoke`;
// what the *plugin* may do while running is decided entirely by its own
// principal, not by the caller's grants (WP-3.1-decisions.md §3) — so a user
// with `plugin:invoke` and nothing else cannot use a plugin as a way to reach
// data they are not allowed to read.
func callPlugin(db *storage.DB, host plugins.Host) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectPlugin, actionInvoke); err != nil {
			fail(w, r, err)
			return
		}
		p, err := plugins.Get(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, plugins.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "unknown plugin",
					"this tenant has no plugin by that id", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		var body struct {
			Input string `json:"input"`
		}
		if r.ContentLength > 0 && !decodeJSON(w, r, &body) {
			return
		}
		out, err := plugins.Call(r.Context(), host, tenant, p, r.PathValue("fn"), []byte(body.Input))
		if err != nil {
			if errors.Is(err, plugins.ErrFunctionNotDeclared) {
				writeProblem(w, http.StatusNotFound, "unknown function",
					"the plugin's manifest does not declare that function", r.URL.Path)
				return
			}
			// A plugin that traps, times out, or is refused a host call is a
			// failure of the plugin, not of the server: 422 with the reason,
			// never a 500 that looks like LastERP broke.
			writeProblem(w, http.StatusUnprocessableEntity, "plugin call failed", err.Error(), r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"output": string(out)})
	}
}

// pluginHost assembles what a plugin invocation is allowed to reach. The
// object map is built from the same schemas the CRUD routes use, so a plugin
// sees exactly the objects the API does — and authz still decides each call.
func pluginHost(db *storage.DB, objects []*metadata.EffectiveSchema, keys secrets.KeySource) plugins.Host {
	cruds := make(map[string]*metadata.CRUD, len(objects))
	for _, s := range objects {
		crud, err := metadata.NewCRUD(s)
		if err != nil {
			continue // event-sourced: no CRUD surface, so nothing for a plugin to address
		}
		cruds[s.ObjectName] = crud
	}
	return plugins.Host{
		DB: db, Objects: cruds, Keys: keys, Limits: plugins.DefaultLimits,
		HTTP: plugins.HTTPPolicy{AllowPrivateNetworks: allowPrivateOutbound()},
	}
}

// allowPrivateOutbound reads the one outbound dial an operator has
// (docs/09 §Plugin outbound HTTP). It is deliberately a deployment setting and
// not a manifest capability: "may plugins reach this network" is a fact about
// where LastERP runs, and a plugin asking for it would be the plugin deciding.
func allowPrivateOutbound() bool {
	ok, err := strconv.ParseBool(os.Getenv("LASTERP_PLUGIN_HTTP_ALLOW_PRIVATE"))
	return err == nil && ok
}

// hookWarnings is the plain-language cost of what an administrator just
// approved. It is returned by the install call rather than buried in a doc: the
// person who installs a plugin is the one who can decide not to, and the person
// who feels the latency usually is not them (WP-3.1b-decisions.md §7).
func hookWarnings(p *plugins.Installed) []string {
	var out []string
	for _, h := range p.Manifest.Hooks {
		if w := h.LatencyWarning(); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// pluginDeadLetters lists deliveries that failed every retry. They are visible
// rather than merely logged for the reason INV-S4 gives about rejected offline
// commands: work that vanished is worse than work that failed loudly.
func pluginDeadLetters(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectPlugin, actionManage); err != nil {
			fail(w, r, err)
			return
		}
		letters, err := plugins.DeadLetters(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		if letters == nil {
			letters = []plugins.DeadLetter{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": letters})
	}
}

// resetPluginBreaker closes a tripped breaker. `manage`, not `invoke`: deciding
// that a plugin is fixed is an administrator's call, and it is audited.
func resetPluginBreaker(db *storage.DB, dispatcher *plugins.Dispatcher) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectPlugin, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		id := r.PathValue("id")
		if err := plugins.ResetBreaker(r.Context(), db, tenant, id, string(actor.UserID)); err != nil {
			if errors.Is(err, plugins.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "unknown plugin",
					"this tenant has no plugin by that id", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		dispatcher.Forget(tenant)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "breaker-closed"})
	}
}

// StartHookRunner runs async hook delivery until ctx is cancelled, returning a
// function that waits for it to stop.
//
// It sweeps on a ticker rather than subscribing to the change feed's notifier:
// subscribing needs the tenant list anyway, and a sweep is the version with no
// wake-up to miss. The ceiling is honest — one pass is O(tenants with plugins)
// per tick, so a deployment with thousands of tenants wants the notifier path
// instead; `tenants` is a global table (ADR-005) so listing it is one query.
func StartHookRunner(ctx context.Context, db *storage.DB, dispatcher *plugins.Dispatcher, every time.Duration) func() {
	if every <= 0 {
		every = 2 * time.Second
	}
	runner := plugins.NewRunner(dispatcher.Host(), dispatcher.Stats())
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
					log.Printf("plugin hook runner: list tenants: %v", err)
					continue
				}
				for _, tenant := range tenants {
					if _, err := runner.Deliver(ctx, tenant); err != nil && ctx.Err() == nil {
						// One tenant's delivery failing must not stop every
						// other tenant's: the failure is already recorded on
						// the plugin (breaker) or filed as a dead letter.
						log.Printf("plugin hook runner: deliver for tenant: %v", err)
					}
				}
			}
		}
	}()
	return func() { <-done }
}

// listTenants reads the global tenant root table (ADR-005: tenants are not
// themselves tenant-scoped, so this needs no tenant context).
func listTenants(ctx context.Context, db *storage.DB) ([]tenancy.ID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []tenancy.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, tenancy.ID(id))
	}
	return out, rows.Err()
}

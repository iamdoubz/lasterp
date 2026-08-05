// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"encoding/base64"
	"errors"
	"net/http"

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
func pluginActions(db *storage.DB, host plugins.Host) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/plugins", Object: objectPlugin,
			Summary: "List installed plugins and the authority each was granted",
			Handler: listPlugins(db)},
		{Method: "POST", Path: "/api/v1/plugins", Object: objectPlugin, Write: true,
			Summary: "Install a plugin, approving the capabilities its manifest requests",
			Handler: installPlugin(db)},
		{Method: "DELETE", Path: "/api/v1/plugins/{id}", Object: objectPlugin, Write: true,
			Summary: "Uninstall a plugin and revoke the authority it was granted",
			Handler: uninstallPlugin(db)},
		{Method: "POST", Path: "/api/v1/plugins/{id}/call/{fn}", Object: objectPlugin, Write: true,
			Summary: "Call a function the plugin's manifest declares",
			Handler: callPlugin(db, host)},
	}
}

func listPlugins(db *storage.DB) api.HandlerFunc {
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
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// installPlugin approves and stores a plugin. One call decides both halves:
// Authorize is the permission (INV-T2) and the actor it returns is both the
// attribution (INV-T4) and the ceiling — kernel/plugins refuses any capability
// this actor does not itself hold (INV-T3).
func installPlugin(db *storage.DB) api.HandlerFunc {
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
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": p.ID, "version": p.Version, "sha256": p.SHA256, "status": "installed",
		})
	}
}

func uninstallPlugin(db *storage.DB) api.HandlerFunc {
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
	return plugins.Host{DB: db, Objects: cruds, Keys: keys, Limits: plugins.DefaultLimits}
}

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// objectSync is the authz object guarding the change feed. The feed spans every
// object a tenant holds, so it gets a permission of its own rather than
// inheriting one: a principal who may read invoices has not thereby been given
// a firehose of every change in the tenant.
const objectSync = "sync"

// defaultFeedLimit / maxFeedLimit bound a page. The cap exists because the
// caller picks the page size and an unbounded one is a memory amplifier: one
// request would materialise the whole feed.
const (
	defaultFeedLimit = 100
	maxFeedLimit     = 1000
)

// syncActions exposes the downstream sync surface (commandment 2: every
// capability reachable via API). Read-only and cursored — the client passes
// back the cursor it last applied.
//
// The streaming transport docs/04 describes (WebSocket / gRPC-web, live push
// within ~1s) is deliberately not here: polling converges, and push only
// changes latency. See WP-2.1-decisions.md §7.
func syncActions(db *storage.DB, objects []*metadata.EffectiveSchema, reg *capability.Registry) []api.Action {
	res := newResolver(db, objects, reg)
	return []api.Action{
		{Method: "GET", Path: "/api/v1/sync/changes", Object: objectSync,
			Summary: "Read the tenant's change feed from a cursor, optionally with row state",
			Handler: readChanges(db, res)},
		{Method: "GET", Path: "/api/v1/sync/snapshot", Object: objectSync,
			Summary: "Page one object's rows for initial replica hydration",
			Handler: readSnapshot(db, res)},
	}
}

// resolver turns object names into the CRUD engine that reads them, skipping
// objects a tenant has switched off and objects that have no CRUD surface at
// all (event-sourced streams — a replica of a stream is a different shape, and
// WP-2.2-decisions.md §9 defers it).
type resolver struct {
	db      *storage.DB
	cruds   map[string]*metadata.CRUD
	checker capability.GatewayChecker
}

func newResolver(db *storage.DB, objects []*metadata.EffectiveSchema, reg *capability.Registry) *resolver {
	cruds := make(map[string]*metadata.CRUD, len(objects))
	for _, s := range objects {
		crud, err := metadata.NewCRUD(s)
		if err != nil {
			continue // event-sourced: no CRUD surface
		}
		cruds[s.ObjectName] = crud
	}
	return &resolver{db: db, cruds: cruds, checker: capability.GatewayChecker{Reg: reg, DB: db}}
}

// crudFor returns the engine for object, or nil when the object is unknown,
// event-sourced, or belongs to a module this tenant has disabled.
func (res *resolver) crudFor(r *http.Request, tenant tenancy.ID, object string) (*metadata.CRUD, error) {
	crud, ok := res.cruds[object]
	if !ok {
		return nil, nil
	}
	enabled, _, err := res.checker.Enabled(r.Context(), tenant, object)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	return crud, nil
}

func readChanges(db *storage.DB, res *resolver) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		// Explicit authorization, not merely authentication (INV-T2). Until
		// WP-2.4 computes per-device scopes, holding sync:read means seeing the
		// tenant's whole feed, so it is a privileged grant — see the scope note
		// in WP-2.1-decisions.md §4.
		if _, err := authz.Authorize(r.Context(), db, objectSync, "read"); err != nil {
			fail(w, r, err)
			return
		}

		after, err := intParam(r, "after", 0)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", err.Error(), r.URL.Path)
			return
		}
		limit, err := feedLimit(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid limit", err.Error(), r.URL.Path)
			return
		}

		changes, err := changefeed.Read(r.Context(), db, tenant, after, limit)
		if err != nil {
			fail(w, r, err)
			return
		}

		// cursor is the position to resume from. It is the last change's
		// cursor, or the caller's own when the page is empty — never zero,
		// which would silently rewind a caught-up client to the beginning.
		cursor := after
		if n := len(changes); n > 0 {
			cursor = changes[n-1].Cursor
		}
		body := map[string]any{"data": changes, "cursor": cursor}

		if r.URL.Query().Get("include") == "rows" {
			rows, err := materialize(r, res, tenant, changes)
			if err != nil {
				fail(w, r, err)
				return
			}
			body["rows"] = rows
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// materialize resolves feed pointers into current row state, grouped by object.
//
// Two properties this deliberately has, both argued in WP-2.2-decisions.md:
//
//   - **It re-authorizes per object kind** (§3). WP-2.1's threat notes leaned on
//     the pointer design to bound an over-broad sync:read to "object names and
//     row ids, not row contents"; materialising removes that bound, so it
//     cannot inherit the feed's single sync:read grant. A caller without
//     Invoice:read gets invoice pointers and no invoice rows — the pointer is
//     the weaker disclosure and suppressing it would only make cursors look
//     non-contiguous.
//   - **It returns current state, not state-at-cursor** (§2). A row that changed
//     at cursors 5 and 9 resolves to 9's state at either. Applying row state is
//     idempotent and last-writer-wins per row, so a client can end up holding
//     state newer than its own cursor claims — ahead, never behind, which is
//     the safe direction. The replica converges to current server state, which
//     is what the AC asks for; it is not a time machine, and reconstructing
//     history is what the event log on the server is for.
func materialize(r *http.Request, res *resolver, tenant tenancy.ID, changes []changefeed.Change) (map[string][]metadata.Record, error) {
	// Group ids by object, preserving first-seen order and de-duplicating: a
	// row touched three times in one page is one lookup, not three.
	idsByObject := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, c := range changes {
		if c.Source != changefeed.SourceAudit {
			// Event-sourced pointers have no CRUD row to resolve to
			// (decisions §9). They stay in `data` so the cursor stays honest.
			continue
		}
		if seen[c.Object] == nil {
			seen[c.Object] = map[string]bool{}
		}
		if seen[c.Object][c.RefID] {
			continue
		}
		seen[c.Object][c.RefID] = true
		idsByObject[c.Object] = append(idsByObject[c.Object], c.RefID)
	}

	out := make(map[string][]metadata.Record, len(idsByObject))
	for object, ids := range idsByObject {
		crud, err := res.crudFor(r, tenant, object)
		if err != nil {
			return nil, err
		}
		if crud == nil {
			continue // unknown, event-sourced, or module disabled for this tenant
		}
		records, err := crud.GetMany(r.Context(), res.db, tenant, ids)
		if errors.Is(err, authz.ErrPermissionDenied) {
			// Not an error for the caller: they may read the feed but not this
			// object. Omitting the rows *is* the answer (§3).
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(records) > 0 {
			out[object] = records
		}
	}
	return out, nil
}

// readSnapshot pages one object's rows for initial hydration, and reports the
// feed cursor to resume from afterwards.
//
// The cursor is the feed's high-water mark *now*, and the client keeps the one
// from its first page. Paging a table while it is being written can show a mix
// of old and new rows; that is repaired rather than prevented, because every
// change after the recorded cursor is in the feed and applying it is
// idempotent. The alternative — one repeatable-read snapshot spanning every
// page — holds a transaction open for the length of a full hydration
// (decisions §4).
func readSnapshot(db *storage.DB, res *resolver) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectSync, "read"); err != nil {
			fail(w, r, err)
			return
		}

		object := r.URL.Query().Get("object")
		if object == "" {
			writeProblem(w, http.StatusBadRequest, "invalid snapshot parameters",
				"object query parameter is required", r.URL.Path)
			return
		}
		limit, err := feedLimit(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid limit", err.Error(), r.URL.Path)
			return
		}

		crud, err := res.crudFor(r, tenant, object)
		if err != nil {
			fail(w, r, err)
			return
		}
		if crud == nil {
			writeProblem(w, http.StatusNotFound, "unknown object",
				"no replicable object named "+object, r.URL.Path)
			return
		}

		records, err := crud.ListPage(r.Context(), res.db, tenant, r.URL.Query().Get("after"), limit)
		if err != nil {
			fail(w, r, err)
			return
		}

		cursor, err := changefeed.HighWater(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}

		// next is the id to page from, empty when this page is the last one.
		next := ""
		if len(records) == limit {
			if id, ok := records[len(records)-1]["id"].(string); ok {
				next = id
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": object, "data": records, "cursor": cursor, "next": next,
		})
	}
}

func feedLimit(r *http.Request) (int, error) {
	limit, err := intParam(r, "limit", defaultFeedLimit)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > maxFeedLimit {
		return 0, errors.New("limit must be between 1 and " + strconv.Itoa(maxFeedLimit))
	}
	return int(limit), nil
}

func intParam(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

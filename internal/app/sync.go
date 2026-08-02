// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"strconv"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
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

// syncActions exposes the change feed over HTTP (commandment 2: every
// capability reachable via API). Read-only and cursored — the client passes
// back the cursor it last applied.
//
// The streaming transport docs/04 describes (WebSocket / gRPC-web, live push
// within ~1s) is deliberately not here: WP-2.2 builds the replica that consumes
// it, and framing designed before its only consumer exists gets designed twice.
// This route plus the in-process notifier covers everything a poller needs, and
// the ordering and resume guarantees are identical either way.
func syncActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/sync/changes", Object: objectSync,
			Summary: "Read the tenant's change feed from a cursor", Handler: readChanges(db)},
	}
}

func readChanges(db *storage.DB) api.HandlerFunc {
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
		limit, err := intParam(r, "limit", defaultFeedLimit)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid limit", err.Error(), r.URL.Path)
			return
		}
		if limit <= 0 || limit > maxFeedLimit {
			writeProblem(w, http.StatusBadRequest, "invalid limit",
				"limit must be between 1 and "+strconv.Itoa(maxFeedLimit), r.URL.Path)
			return
		}

		changes, err := changefeed.Read(r.Context(), db, tenant, after, int(limit))
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
		writeJSON(w, http.StatusOK, map[string]any{"data": changes, "cursor": cursor})
	}
}

func intParam(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

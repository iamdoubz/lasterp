// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"sort"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// computeScope returns the sync scope of the actor on r: the sorted set of
// scope keys whose changes this principal may replicate (docs/04 §Concepts,
// WP-2.4-decisions.md §1).
//
// A scope key is an object name, which is what WP-2.1 writes onto every feed
// entry. So the computation is an intersection of three things that all already
// exist, rather than a new entitlement model beside the one the product uses:
//
//  1. the objects the actor holds a `read` grant on, through any role;
//  2. the objects that are replicable at all — a CRUD surface exists, which
//     excludes event-sourced streams (WP-2.2-decisions.md §9);
//  3. the objects whose module this tenant has enabled.
//
// What it deliberately is not: docs/04's "my region's customers, open documents
// last 24 months". Row-level narrowing needs condition evaluation, which authz
// does not have (ErrConditionNotSupported, WP-0.3) — decisions §1. When it
// arrives it subdivides these keys rather than replacing them.
//
// Being a *set* is why this uses authz.GrantedObjects rather than a Can per
// object: the question is asked on every feed page, and one query beats a dozen.
// It is not an authorization decision — CRUD.GetMany still authorizes each
// object it materialises (INV-T2, WP-2.2-decisions.md §3). This narrows what a
// caller is *offered*; it never widens it.
//
// ponytail: recomputed per request — one grants query plus one module-enabled
// query per granted object, on a path a client polls. Deliberately not cached:
// a stale scope is a principal still being served data after their entitlement
// was withdrawn, which is the one failure this WP exists to prevent, and
// correct invalidation is a bigger thing than the query it saves. If it ever
// measures hot, the upgrade is a request-scoped memo on capability.Enabled
// (whose real granularity is the module, not the object) before it is anything
// that outlives a request.
func computeScope(r *http.Request, res *resolver, tenant tenancy.ID) ([]string, error) {
	actor, err := authz.ActorFromContext(r.Context())
	if err != nil {
		return nil, err
	}
	granted, err := authz.GrantedObjects(r.Context(), res.db, actor, "read")
	if err != nil {
		return nil, err
	}

	scope := make([]string, 0, len(granted))
	for _, object := range granted {
		crud, err := res.crudFor(r, tenant, object)
		if err != nil {
			return nil, err
		}
		if crud == nil {
			continue // unknown, event-sourced, or module disabled for this tenant
		}
		scope = append(scope, object)
	}
	sort.Strings(scope)
	return scope, nil
}

// readScope serves the scope itself, so a client can re-shape to it.
//
// It returns the list rather than a version stamp. docs/04 §Downstream 4 says
// the server "bumps scope version"; a version is a change-detector, and this
// client already fetches /meta/objects on every reconnect, so a dozen object
// names cost the same round trip and answer "which ones" as well as "did it
// change" (decisions §2). The client diffs the list against what it holds, which
// *is* the re-shape.
func readScope(db *storage.DB, res *resolver) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		// The same grant that guards the feed guards the shape of it: knowing
		// which objects you may replicate is knowing what you may read.
		if _, err := authz.Authorize(r.Context(), db, objectSync, "read"); err != nil {
			fail(w, r, err)
			return
		}
		scope, err := computeScope(r, res, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": emptyIfNil(scope)})
	}
}

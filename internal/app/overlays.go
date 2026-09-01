// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"io"
	"net/http"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// objectOverlay is the authz object guarding tenant customization. It is its
// own permission rather than a consequence of holding CRUD on the object being
// customized: `Contact:update` is the right to edit a contact, and adding a
// field to every contact in the tenant is a different power held by a different
// person.
const objectOverlay = "Overlay"

// maxOverlayBytes caps an overlay document. Generous for a document whose
// realistic size is a dozen lines, and small enough that the request body
// cannot be a memory attack.
const maxOverlayBytes = 256 << 10

// overlayActions is the tenant's customization surface — ADR-006's "all
// metadata is data: exportable as customization packages, versionable in git",
// which needs a way in and a way out to be true at all.
//
// Hand-registered actions rather than generic CRUD, for the reason automations
// are: an overlay is a document that must merge against a live schema before it
// is stored, and a generic PATCH would let half a customization land.
func overlayActions(db *storage.DB, objects []*metadata.EffectiveSchema) []api.Action {
	cores := customizableObjects(objects)
	return []api.Action{
		{Method: "GET", Path: "/api/v1/meta/overlays", Object: objectOverlay,
			Summary: "List this tenant's metadata overlays",
			Handler: listTenantOverlays(db)},
		{Method: "PUT", Path: "/api/v1/meta/overlays/{object}", Object: objectOverlay, Write: true,
			Summary: "Create or replace this tenant's overlay for one object",
			Handler: putTenantOverlay(db, cores)},
		{Method: "DELETE", Path: "/api/v1/meta/overlays/{object}", Object: objectOverlay, Write: true,
			Summary: "Remove this tenant's overlay for one object",
			Handler: deleteTenantOverlay(db)},
	}
}

// customizableObjects is what an overlay may target: the core object behind
// each schema the gateway serves as generic CRUD.
//
// That set, and not the wider one the plugin host addresses, because those are
// the objects whose write path resolves the tenant's effective schema. Adding a
// field to an Invoice would produce a field invoicing's posting pipeline — which
// builds its own schema — could never write (WP-3.2c-decisions.md §2).
func customizableObjects(objects []*metadata.EffectiveSchema) map[string]*metadata.Object {
	cores := make(map[string]*metadata.Object, len(objects))
	for _, s := range objects {
		core := s.Object
		cores[s.ObjectName] = &core
	}
	return cores
}

// overlayJSON projects one stored overlay. The definition travels as the YAML
// it was stored as, not as re-marshalled JSON: an export that does not
// round-trip byte-for-byte is not a customization package, it is a lossy
// screenshot of one.
func overlayJSON(s metadata.StoredOverlay) map[string]any {
	return map[string]any{
		"object":     s.ObjectName,
		"layer":      string(s.Layer),
		"source":     s.Source,
		"definition": string(s.Definition),
		"updated_at": s.UpdatedAt,
		"updated_by": s.UpdatedBy,
	}
}

func listTenantOverlays(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectOverlay, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		// Plugin layers are listed alongside the tenant's own. An administrator
		// asking "what has been done to my schema" needs the whole stack;
		// showing only the half they wrote themselves is how a plugin's field
		// becomes a mystery.
		list, err := metadata.ListOverlays(r.Context(), db, actor.TenantID)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, s := range list {
			out = append(out, overlayJSON(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"overlays": out})
	}
}

func putTenantOverlay(db *storage.DB, cores map[string]*metadata.Object) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectOverlay, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		core, ok := cores[r.PathValue("object")]
		if !ok {
			// Not a 422: the route named an object that does not exist, which
			// is a 404 about the path, not a complaint about the body. A
			// *custom* object an overlay would define is out of scope
			// (WP-3.2c-decisions.md §2).
			writeProblem(w, http.StatusNotFound, "unknown object",
				"no shipped object by that name; overlays customize existing objects", r.URL.Path)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxOverlayBytes))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "unreadable body", err.Error(), r.URL.Path)
			return
		}

		// The tenant layer, always: this route is the administrator's, and a
		// caller must not be able to write a *plugin's* layer — that would let
		// an overlay outlive the uninstall that was supposed to remove it, and
		// let a tenant forge authority a plugin was never approved for.
		err = metadata.SaveOverlay(r.Context(), db, actor.TenantID, core,
			metadata.LayerTenant, metadata.TenantSource, body, string(actor.UserID))
		if err != nil {
			// A document that will not merge is the caller's mistake: 422 with
			// the merge error, which names the field and the offending value —
			// "would gain [banana]" is actionable, "invalid overlay" is not.
			if overlayRefused(err) {
				writeProblem(w, http.StatusUnprocessableEntity, "invalid overlay", err.Error(), r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": core.ObjectName, "layer": string(metadata.LayerTenant)})
	}
}

func deleteTenantOverlay(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectOverlay, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		// Only the tenant's own layer, mirroring the PUT: a plugin's overlay is
		// removed by uninstalling the plugin, not by an admin reaching past it.
		if err := metadata.DeleteOverlay(r.Context(), db, actor.TenantID, r.PathValue("object"),
			metadata.LayerTenant, metadata.TenantSource); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// overlayRefused reports whether err is a document the caller got wrong, as
// opposed to a fault. Every refusal SaveOverlay can raise about the document
// itself is listed, so a new one is a compile-time-visible omission rather than
// a silent 500.
func overlayRefused(err error) bool {
	return errors.Is(err, metadata.ErrInvalidObject) ||
		errors.Is(err, metadata.ErrOverlayTarget) ||
		errors.Is(err, metadata.ErrOverlayConflict) ||
		errors.Is(err, metadata.ErrOptionSetWidened) ||
		errors.Is(err, metadata.ErrPermissionFloorLowered)
}

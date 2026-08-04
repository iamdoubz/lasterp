// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// objectDevice is the authz object guarding device management. Separate from
// `sync` because the two are different powers: `sync:read` is the right to
// follow your own scope, `device:manage` is the right to destroy what somebody
// else's machine holds.
const objectDevice = "device"

// deviceActions is the management surface for registered devices (WP-2.5,
// docs/08 §Data protection).
//
// Registration is deliberately absent: it happens implicitly at session issue
// (identity.RegisterDevice), because an enrolment call a client can skip is one
// that leaves an untracked replica — WP-2.5-decisions.md §1. What an
// administrator needs, and what "every capability reachable via API" therefore
// requires here, is the ability to *see* devices and *act* on them.
func deviceActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/devices", Object: objectDevice,
			Summary: "List the tenant's registered devices",
			Handler: listDevices(db)},
		{Method: "POST", Path: "/api/v1/devices/{id}/wipe", Object: objectDevice, Write: true,
			Summary: "Remotely wipe a device's local replica at its next connect",
			Handler: wipeDevice(db)},
		{Method: "POST", Path: "/api/v1/devices/{id}/revoke", Object: objectDevice, Write: true,
			Summary: "Stop a device being issued new sessions",
			Handler: revokeDevice(db)},
	}
}

func listDevices(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectDevice, "read"); err != nil {
			fail(w, r, err)
			return
		}
		devices, err := identity.ListDevices(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(devices))
		for _, d := range devices {
			out = append(out, deviceJSON(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// wipeDevice marks a device for remote wipe. The wipe itself happens on the
// device, at its next authenticated request — there is no push channel and
// docs/04 §Downstream 4 never claimed one ("honored at connect").
//
// It requires `manage`, not `update`: this is the most destructive action in
// the product that does not touch the ledger, and it is irreversible from the
// server's side — once delivered, the data is gone.
func wipeDevice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		act(w, r, db, tenant, identity.WipeDevice, "wiped")
	}
}

func revokeDevice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		act(w, r, db, tenant, identity.RevokeDevice, "revoked")
	}
}

// act is the shared body of the two state-changing endpoints: authorize,
// resolve the device (404 if this tenant has no such device — RLS means the
// lookup cannot see another tenant's), apply, and echo the new state.
func act(
	w http.ResponseWriter, r *http.Request, db *storage.DB, tenant tenancy.ID,
	apply func(context.Context, *storage.DB, tenancy.ID, string, string) error, status string,
) {
	// One call decides both halves: Authorize is INV-T2 (an explicit permission
	// grant) and the actor it returns is INV-T4 (the mutation is attributable).
	// kernel/identity cannot read the actor itself — authz imports it, not the
	// other way round — so it is handed down explicitly.
	actor, err := authz.Authorize(r.Context(), db, objectDevice, actionManage)
	if err != nil {
		fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeProblem(w, http.StatusBadRequest, "invalid device", "device id is required", r.URL.Path)
		return
	}
	if _, err := identity.GetDevice(r.Context(), db, tenant, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "unknown device",
				"no registered device with that id", r.URL.Path)
			return
		}
		fail(w, r, err)
		return
	}
	if err := apply(r.Context(), db, tenant, id, string(actor.UserID)); err != nil {
		fail(w, r, err)
		return
	}
	device, err := identity.GetDevice(r.Context(), db, tenant, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	body := deviceJSON(device)
	body["status"] = status
	writeJSON(w, http.StatusOK, body)
}

// deviceJSON renders a device for the wire.
//
// wipe_delivered_at is exposed because an administrator who wipes a stolen
// laptop needs to know whether the instruction actually reached it. The name is
// the honest one: it records that the device was *told*, not that it obeyed —
// nothing can prove a remote client deleted anything (decisions §4).
func deviceJSON(d identity.Device) map[string]any {
	return map[string]any{
		"id":                d.ID,
		"user_id":           string(d.UserID),
		"label":             d.Label,
		"created_at":        d.CreatedAt,
		"last_seen_at":      d.LastSeenAt,
		"revoked_at":        nullableTime(d.RevokedAt),
		"wipe_requested_at": nullableTime(d.WipeRequestedAt),
		"wipe_delivered_at": nullableTime(d.WipeDeliveredAt),
	}
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

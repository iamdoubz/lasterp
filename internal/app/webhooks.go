// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/outbound"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The outbound destination registry (WP-3.3c).
//
// A plugin's outbound allowlist is its manifest, approved once at install. An
// automation has no manifest, so what an administrator approves is a row here,
// and an automation's `webhook` action names it by id.
//
// **`Webhook:manage` is not `Automation:manage`.** Deciding where this
// deployment may call out and writing a rule that calls out are different
// powers; folded into one, anybody who can write an automation picks the host,
// which is the SSRF-and-exfiltration primitive the WP exists to avoid
// (docs/notes/WP-3.3c-decisions.md §1).
//
// The destination's URL is **not** here: it lives in the vault, because for a
// whole class of webhook APIs the path is the credential (INV-K1, §2). These
// routes never return it, in the same shape as the secrets API — which is why
// there is no reveal endpoint and `TestNoRouteReturnsAWebhookURL` enumerates
// the live mux rather than trusting review.
func webhookActions(db *storage.DB, keys secrets.KeySource) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/webhooks/destinations", Object: outbound.ObjectWebhook,
			Summary: "List the tenant's approved outbound destinations — hosts and metadata, never URLs",
			Handler: listDestinations(db)},
		{Method: "PUT", Path: "/api/v1/webhooks/destinations/{id}", Object: outbound.ObjectWebhook, Write: true,
			Summary: "Register or replace an outbound destination",
			Handler: putDestination(db, keys)},
		{Method: "DELETE", Path: "/api/v1/webhooks/destinations/{id}", Object: outbound.ObjectWebhook, Write: true,
			Summary: "Remove an outbound destination",
			Handler: deleteDestination(db)},
	}
}

func listDestinations(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, outbound.ObjectWebhook, outbound.ActionManage); err != nil {
			fail(w, r, err)
			return
		}
		list, err := outbound.ListDestinations(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, d := range list {
			out = append(out, destinationJSON(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

func putDestination(db *storage.DB, keys secrets.KeySource) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, outbound.ObjectWebhook, outbound.ActionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "unreadable body", err.Error(), r.URL.Path)
			return
		}
		var in struct {
			Host        string `json:"host"`
			SecretName  string `json:"secret_name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid body", err.Error(), r.URL.Path)
			return
		}
		d, err := outbound.Register(r.Context(), db, keys, tenant, outbound.Destination{
			ID: r.PathValue("id"), Host: in.Host, SecretName: in.SecretName, Description: in.Description,
		}, string(actor.UserID))
		if err != nil {
			// A destination whose host is malformed, whose secret is missing,
			// or whose stored URL points somewhere else is the caller's
			// mistake, not a fault — and the message names which.
			writeProblem(w, http.StatusUnprocessableEntity, "invalid destination", err.Error(), r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, destinationJSON(*d))
	}
}

func deleteDestination(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, outbound.ObjectWebhook, outbound.ActionManage); err != nil {
			fail(w, r, err)
			return
		}
		id := r.PathValue("id")
		if _, err := outbound.GetDestination(r.Context(), db, tenant, id); err != nil {
			if errors.Is(err, outbound.ErrNoDestination) {
				writeProblem(w, http.StatusNotFound, "unknown destination",
					"this tenant has no outbound destination by that id", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		if err := outbound.DeleteDestination(r.Context(), db, tenant, id); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
	}
}

// destinationJSON is the whole of what a destination may say about itself. The
// secret's *name* is metadata an administrator needs to see the wiring; its
// value is the URL and is never here (INV-K1).
func destinationJSON(d outbound.Destination) map[string]any {
	return map[string]any{
		"id":          d.ID,
		"host":        d.Host,
		"secret_name": d.SecretName,
		"description": d.Description,
		"created_at":  d.CreatedAt,
		"created_by":  d.CreatedBy,
	}
}

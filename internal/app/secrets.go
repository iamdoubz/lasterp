// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"net/http"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// objectSecret is the authz object guarding the vault. One permission, not a
// CRUD set: there is no "read a secret" power to grant, because there is no
// route that returns one (WP-3.0-decisions.md §4).
const objectSecret = "secret"

// secretActions is the vault's management surface (WP-3.0, docs/08 §Data
// protection).
//
// **There is no route that returns a secret's value, and adding one is a
// breach of INV-K1, not a feature.** The person who sets a credential already
// holds it; the server uses it on the tenant's behalf. A reveal endpoint turns
// the vault into a credential-exfiltration API reachable with one stolen
// session. TestNoSecretRevealEndpointExists enforces this against the live mux
// rather than against reviewer memory.
func secretActions(db *storage.DB, src secrets.KeySource) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/secrets", Object: objectSecret,
			Summary: "List the tenant's stored secrets — names and metadata, never values",
			Handler: listSecrets(db)},
		{Method: "PUT", Path: "/api/v1/secrets/{name}", Object: objectSecret, Write: true,
			Summary: "Store or replace a secret's value",
			Handler: putSecret(db, src)},
		{Method: "DELETE", Path: "/api/v1/secrets/{name}", Object: objectSecret, Write: true,
			Summary: "Delete a secret",
			Handler: deleteSecret(db)},
	}
}

func listSecrets(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectSecret, actionManage); err != nil {
			fail(w, r, err)
			return
		}
		stored, err := secrets.List(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(stored))
		for _, m := range stored {
			out = append(out, map[string]any{
				"name":        m.Name,
				"description": m.Description,
				// The key id is metadata an operator needs to see a rotation
				// through: it names which KEK the row is wrapped under, and says
				// nothing about the value.
				"key_id":     m.KeyID,
				"created_at": m.CreatedAt,
				"updated_at": m.UpdatedAt,
				"updated_by": m.UpdatedBy,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// putSecret stores a value. Idempotency is the gateway's, as for every Write
// action; replacing a secret is idempotent anyway.
func putSecret(db *storage.DB, src secrets.KeySource) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		// One call decides both halves: Authorize is the permission (INV-T2) and
		// the actor it returns is the attribution (INV-T4) — kernel/secrets
		// cannot read the actor itself, since authz imports identity and not the
		// other way round, so it is handed down explicitly (same shape as
		// devices.go).
		actor, err := authz.Authorize(r.Context(), db, objectSecret, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		if src == nil {
			noKeySource(w, r)
			return
		}
		name := r.PathValue("name")
		if !secrets.ValidName(name) {
			writeProblem(w, http.StatusBadRequest, "invalid secret name",
				"a secret name is 1-128 characters of a-z, 0-9, dot, dash or underscore", r.URL.Path)
			return
		}
		var body struct {
			Value       string `json:"value"`
			Description string `json:"description"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Value == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "empty secret",
				"value is required; delete the secret instead of storing an empty one", r.URL.Path)
			return
		}
		if err := secrets.Put(r.Context(), db, src, tenant, name, body.Description,
			[]byte(body.Value), string(actor.UserID)); err != nil {
			fail(w, r, err)
			return
		}
		// The response echoes metadata only — a write that replied with what it
		// stored would be a reveal endpoint with extra steps.
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "stored"})
	}
}

func deleteSecret(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.Authorize(r.Context(), db, objectSecret, actionManage)
		if err != nil {
			fail(w, r, err)
			return
		}
		name := r.PathValue("name")
		if err := secrets.Delete(r.Context(), db, tenant, name, string(actor.UserID)); err != nil {
			if errors.Is(err, secrets.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "unknown secret",
					"this tenant has no secret by that name", r.URL.Path)
				return
			}
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "deleted"})
	}
}

// noKeySource is what every write answers when the deployment configured no
// key source. It is 503 rather than 500: nothing is broken, a deployment step
// is missing, and the operator needs to be told which one (decisions §3).
//
// Reads (List, Delete) work without a key source, because listing names and
// removing rows never touches key material. An operator who has lost the key
// file can still see what they lost and clear it.
func noKeySource(w http.ResponseWriter, r *http.Request) {
	writeProblemTyped(w, "secrets-no-key-source", http.StatusServiceUnavailable,
		"secrets vault unavailable",
		"this deployment has no secrets key source: set "+secrets.EnvKeyFile+
			" to a key file (`lasterp secrets init`) and restart", r.URL.Path)
}

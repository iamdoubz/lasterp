//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/automations"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.3b's automation e2e: the surface an administrator actually uses, over
// the *real* CRUD engines rather than the kernel suite's in-memory fake.
//
// That difference is the point of this file. kernel/automations proves the
// engine's logic against an Objects it can control; this proves the adapter
// wired in internal/app really goes through metadata.CRUD — the same
// authorization gate, validation and audit as a human write (INV-T2/T4/T5).

// postAutomation saves a definition through the HTTP surface and returns the
// decoded reply.
func (e *env) postAutomation(t *testing.T, yaml string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", e.server.URL+"/api/v1/automations", strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Idempotency-Key", "automation-"+time.Now().Format("150405.000000000"))
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post automation: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// runAutomations drives one pass of the runner the way the server's background
// sweep does, with the real adapters.
func runAutomations(t *testing.T, db *storage.DB, tenant tenancy.ID) int {
	t.Helper()
	host := pluginHost(db, mustCRUDObjects(t), nil)
	runner := automations.NewRunner(db,
		crudObjectsAdapter{db: db, cruds: host.Objects},
		pluginEnqueueAdapter{db: db}, host.Keys, host.HTTP)
	n, err := runner.RunOnce(context.Background(), tenant)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return n
}

func mustCRUDObjects(t *testing.T) []*metadata.EffectiveSchema {
	t.Helper()
	objects, err := crudObjects()
	if err != nil {
		t.Fatalf("crudObjects: %v", err)
	}
	return objects
}

// TestAutomationUpdatesARecordThroughTheRealPipeline is the e2e AC: an
// administrator creates an automation over the API, a write happens through the
// ordinary CRUD surface, and the automation changes the record — attributed to
// itself.
func TestAutomationUpdatesARecordThroughTheRealPipeline(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			status, _ := e.postAutomation(t, `
id: flag-vips
name: Flag VIP contacts
trigger:
  object: Contact
condition: 'record.name == "Ada Lovelace"'
actions:
  - type: field_update
    set:
      locale: "en-GB"
`)
			if status != http.StatusOK {
				t.Fatalf("POST /api/v1/automations = %d, want 200", status)
			}

			// A write through the ordinary surface, by an ordinary user.
			code, _, created := e.call("POST", "/api/v1/contact", e.token, "contact-1",
				map[string]any{"name": "Ada Lovelace", "email": "ada@example.com", "kind": "customer"})
			if code != http.StatusCreated && code != http.StatusOK {
				t.Fatalf("create contact = %d", code)
			}
			id, _ := created["id"].(string)
			if id == "" {
				t.Fatalf("create returned no id: %v", created)
			}
			// And one the condition must not match.
			if code, _, _ := e.call("POST", "/api/v1/contact", e.token, "contact-2",
				map[string]any{"name": "Someone Else", "email": "else@example.com", "kind": "customer"}); code >= 300 {
				t.Fatalf("create second contact = %d", code)
			}

			if fired := runAutomations(t, db, e.tenant); fired != 1 {
				t.Fatalf("automation fired %d times, want 1 — only Ada matches", fired)
			}

			// The record really changed, read back over the API.
			code, _, got := e.call("GET", "/api/v1/contact/"+id, e.token, "", nil)
			if code != http.StatusOK {
				t.Fatalf("get contact = %d", code)
			}
			if got["locale"] != "en-GB" {
				t.Fatalf("the automation did not apply its field_update: %v", got)
			}

			// INV-T4: the audit row names the automation, not the user whose
			// write triggered it. This is the assertion the kernel suite's fake
			// cannot make, because only the real CRUD writes audit rows.
			actors := auditActorsFor(t, e, "Contact")
			if !contains(actors, "automation:flag-vips") {
				t.Fatalf("no audit row attributed to the automation; actors were %v", actors)
			}
		})
	}
}

// A second pass does nothing: the automation does not react to its own write,
// and its field_update is already applied. Without self-suppression this is the
// test that would never terminate in production.
func TestAutomationDoesNotLoopOverTheRealPipeline(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if status, _ := e.postAutomation(t, `
id: loop-guard
name: Would loop
trigger:
  object: Contact
actions:
  - type: field_update
    set:
      locale: "en-GB"
`); status != http.StatusOK {
				t.Fatalf("save automation = %d", status)
			}
			if code, _, _ := e.call("POST", "/api/v1/contact", e.token, "c1",
				map[string]any{"name": "Loop Test", "email": "loop@example.com", "kind": "customer"}); code >= 300 {
				t.Fatalf("create contact = %d", code)
			}

			total := 0
			for pass := 0; pass < 5; pass++ {
				total += runAutomations(t, db, e.tenant)
			}
			if total != 1 {
				t.Fatalf("fired %d times over 5 passes, want 1 — it is reacting to itself", total)
			}
		})
	}
}

// The routes require their permission, and a malformed definition is a 422 that
// says what is wrong rather than a 500.
func TestAutomationRoutesRequireTheirPermission(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			weak := e.issueUser(t, map[string][]string{"Contact": {"read"}})

			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/automations"},
				{"POST", "/api/v1/automations"},
				{"GET", "/api/v1/automations/x"},
				{"DELETE", "/api/v1/automations/x"},
				{"GET", "/api/v1/automations/x/runs"},
			} {
				code, _, _ := e.call(probe.method, probe.path, weak, "probe-"+probe.method+probe.path, map[string]any{})
				if code != http.StatusForbidden && code != http.StatusUnprocessableEntity {
					t.Errorf("%s %s without Automation:manage = %d, want 403", probe.method, probe.path, code)
				}
			}

			// A definition this engine cannot honour is the caller's mistake,
			// and the refusal names the WP that owns the action rather than
			// accepting it as a silent no-op. `webhook` was the example here
			// and shipped in WP-3.3c; `email` still waits on a mailer.
			status, body := e.postAutomation(t, "id: bad\nname: Bad\ntrigger:\n  object: Contact\nactions:\n  - {type: email}\n")
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("malformed automation = %d, want 422", status)
			}
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, "outbound mail") {
				t.Fatalf("the refusal does not name the owner of the deferred action: %v", body)
			}
		})
	}
}

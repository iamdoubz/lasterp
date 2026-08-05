//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/plugins"
)

// WP-3.1b: the hook surface, end to end over the wired product handler.
//
// Invariants: **INV-X2** (structural — no plugin code runs inside a
// transaction, so none can partially commit one; the kernel suite proves the
// dispatch site, this proves the behaviour through the real write path),
// INV-T2 (hook routes are authorized), INV-T4 (breaker transitions are
// attributable), INV-T5 (hook enrichment is re-validated), plus INV-S4's shape
// for dead letters: nothing is dropped silently.

var (
	hookCorpusOnce sync.Once
	hookCorpusWasm []byte
	hookCorpusErr  error
)

func hooksModule(t *testing.T) []byte {
	t.Helper()
	hookCorpusOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lasterp-app-hooks")
		if err != nil {
			hookCorpusErr = err
			return
		}
		out := filepath.Join(dir, "hooks.wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./hooks")
		cmd.Dir = filepath.Join("..", "..", "kernel", "plugins", "testdata")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if combined, err := cmd.CombinedOutput(); err != nil {
			hookCorpusErr = &corpusBuildError{out: string(combined), err: err}
			return
		}
		hookCorpusWasm, hookCorpusErr = os.ReadFile(out)
	})
	if hookCorpusErr != nil {
		t.Fatalf("build hooks plugin: %v", hookCorpusErr)
	}
	return hookCorpusWasm
}

// contactHookManifest hooks the Contact object the product actually serves.
func contactHookManifest(hooks string) string {
	return `
id: com.acme.hooks
version: 1.0.0
functions: [veto, enrich, smuggle, boom, note, spawn]
capabilities:
  objects:
    - {type: Contact, access: read}
    - {type: Contact, access: write}
hooks:
` + hooks
}

// installHookPlugin installs the hook corpus member over HTTP and returns the
// parsed install response.
func installHookPlugin(t *testing.T, e *env, hooks string) map[string]any {
	t.Helper()
	status, body, parsed := e.post("/api/v1/plugins", map[string]any{
		"manifest": contactHookManifest(hooks),
		"module":   base64.StdEncoding.EncodeToString(hooksModule(t)),
	})
	if status != http.StatusCreated {
		t.Fatalf("install hook plugin = %d: %s", status, body)
	}
	return parsed
}

// --- veto, through the real write path ---

func TestVetoingHookRefusesTheWriteWithAUsableError(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installHookPlugin(t, e, "  - {event: Contact.before_create, fn: veto, mode: sync}\n")

			status, body, parsed := e.post("/api/v1/contact", map[string]any{
				"name": "REJECT Industries", "kind": "customer",
			})
			// 422: a well-formed request refused by a business rule, which is
			// what a veto is — not a 500, and not a 400.
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("vetoed write = %d, want 422; body=%s", status, body)
			}
			detail, _ := parsed["detail"].(string)
			if !strings.Contains(detail, "com.acme.hooks") {
				t.Errorf("the refusal must name the plugin so a user can act on it: %s", body)
			}

			// Nothing was written (INV-X2: no partial commit).
			_, listBody, listed := e.get("/api/v1/contact")
			if strings.Contains(string(listBody), "REJECT Industries") {
				t.Errorf("a vetoed write left a row behind: %s", listBody)
			}
			_ = listed

			// A write the hook does not object to still works — a hook surface
			// that blocked everything would pass the test above and be useless.
			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "Perfectly Fine Ltd", "kind": "customer",
			}); status != http.StatusCreated {
				t.Fatalf("an unobjectionable write = %d: %s", status, body)
			}
		})
	}
}

func TestEnrichingHookIsAppliedAndStillValidated(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installHookPlugin(t, e, "  - {event: Contact.before_create, fn: enrich, mode: sync}\n")

			status, body, rec := e.post("/api/v1/contact", map[string]any{
				"name": "Plain Ltd", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create = %d: %s", status, body)
			}
			if got, _ := rec["name"].(string); got != "Plain Ltd (enriched)" {
				t.Errorf("enrichment did not reach the stored record: %q", got)
			}
		})
	}
}

func TestHookCannotEnrichPastTheSchema(t *testing.T) {
	db := sqliteBootDB(t)
	e, _ := seedVault(t, db)
	installHookPlugin(t, e, "  - {event: Contact.before_create, fn: smuggle, mode: sync}\n")

	status, body, _ := e.post("/api/v1/contact", map[string]any{
		"name": "Fine Ltd", "kind": "customer",
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("INV-T5: a hook wrote an out-of-set value; status = %d, body=%s", status, body)
	}
}

// --- the install-time warning the administrator sees ---

func TestInstallShowsWhatTheHookWillCostPerWrite(t *testing.T) {
	db := sqliteBootDB(t)
	e, _ := seedVault(t, db)
	parsed := installHookPlugin(t, e,
		"  - {event: Contact.before_create, fn: veto, mode: sync, timeout_ms: 400}\n")

	warnings, _ := parsed["warnings"].([]any)
	if len(warnings) != 1 {
		t.Fatalf("install returned %d warnings, want one naming the cost", len(warnings))
	}
	w, _ := warnings[0].(string)
	// Plain language, at the moment of approval: the person who installs is the
	// one who can decide not to, and they are usually not the one who will feel
	// the latency (WP-3.1b-decisions.md §7).
	for _, want := range []string{"every write of Contact", "400ms", "slow for everyone"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not mention %q", w, want)
		}
	}
}

func TestHookTimeoutCeilingIsEnforcedAtInstall(t *testing.T) {
	db := sqliteBootDB(t)
	e, _ := seedVault(t, db)
	status, body, _ := e.post("/api/v1/plugins", map[string]any{
		"manifest": contactHookManifest(
			"  - {event: Contact.before_create, fn: veto, mode: sync, timeout_ms: 5000}\n"),
		"module": base64.StdEncoding.EncodeToString(hooksModule(t)),
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a hook over the timeout ceiling installed: %d %s", status, body)
	}
	if !strings.Contains(string(body), "500") {
		t.Errorf("the refusal does not state the ceiling: %s", body)
	}
}

// --- async delivery, the breaker and the dead-letter surface ---

func TestAsyncHookDeliversAndFailuresAreVisible(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e, keys := seedVault(t, db)
			installHookPlugin(t, e, "  - {event: Contact.changed, fn: boom, mode: async}\n")

			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "Will Fail Delivery", "kind": "customer",
			}); status != http.StatusCreated {
				t.Fatalf("create = %d: %s", status, body)
			}

			// One delivery pass, driven directly rather than by waiting on the
			// background sweep: a test that sleeps is a test that flakes.
			objects, err := crudObjects()
			if err != nil {
				t.Fatalf("crudObjects: %v", err)
			}
			runner := plugins.NewRunner(pluginHost(db, objects, keys), nil)
			if _, err := runner.Deliver(ctx, e.tenant); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			status, body, parsed := e.get("/api/v1/plugins/com.acme.hooks/dead-letters")
			if status != http.StatusOK {
				t.Fatalf("dead-letters = %d: %s", status, body)
			}
			data, _ := parsed["data"].([]any)
			if len(data) != 1 {
				t.Fatalf("dead letters = %d, want 1 — a failed delivery vanished; body=%s", len(data), body)
			}
			letter, _ := data[0].(map[string]any)
			if letter["fn"] != "boom" || letter["object"] != "Contact" {
				t.Errorf("dead letter does not describe the failure: %v", letter)
			}
		})
	}
}

func TestBreakerStateAndHookCostAreVisibleToAnAdministrator(t *testing.T) {
	db := sqliteBootDB(t)
	e, _ := seedVault(t, db)
	installHookPlugin(t, e,
		"  - {event: Contact.before_create, fn: boom, mode: sync, on_failure: allow}\n")

	for i := 0; i < plugins.BreakerThreshold+1; i++ {
		if status, body, _ := e.post("/api/v1/contact", map[string]any{
			"name": "Attempt", "kind": "customer",
		}); status != http.StatusCreated {
			t.Fatalf("write %d = %d: %s", i, status, body)
		}
	}

	status, body, parsed := e.get("/api/v1/plugins")
	if status != http.StatusOK {
		t.Fatalf("list plugins = %d: %s", status, body)
	}
	data, _ := parsed["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("plugins listed = %d", len(data))
	}
	row, _ := data[0].(map[string]any)
	if open, _ := row["breaker_open"].(bool); !open {
		t.Errorf("the breaker is not visible as open after repeated failures: %v", row)
	}
	// Cost attribution: a slow or broken plugin must read as *that plugin's*
	// cost, not as "the ERP is slow" (decisions §7).
	hooks, _ := row["hooks"].([]any)
	if len(hooks) == 0 {
		t.Fatal("no per-hook stats reported; a slow plugin would be indistinguishable from a slow product")
	}
	stat, _ := hooks[0].(map[string]any)
	if stat["plugin"] != "com.acme.hooks" || stat["fn"] != "boom" {
		t.Errorf("hook stats are not attributed: %v", stat)
	}

	// And an administrator can close it without waiting out the cooldown.
	if status, body, _ := e.post("/api/v1/plugins/com.acme.hooks/reset-breaker", nil); status != http.StatusOK {
		t.Fatalf("reset-breaker = %d: %s", status, body)
	}
	_, _, parsed = e.get("/api/v1/plugins")
	data, _ = parsed["data"].([]any)
	row, _ = data[0].(map[string]any)
	if open, _ := row["breaker_open"].(bool); open {
		t.Error("the breaker is still open after a reset")
	}
}

func TestHookRoutesRequireTheirPermissions(t *testing.T) {
	db := sqliteBootDB(t)
	e, _ := seedVault(t, db)
	installHookPlugin(t, e, "  - {event: Contact.changed, fn: note, mode: async}\n")
	bare := e.issueUser(t, map[string][]string{"Contact": {"read"}})

	for _, probe := range []struct{ method, path string }{
		{"GET", "/api/v1/plugins/com.acme.hooks/dead-letters"},
		{"POST", "/api/v1/plugins/com.acme.hooks/reset-breaker"},
	} {
		status, body, _ := e.call(probe.method, probe.path, bare, idgen.New(), map[string]any{})
		if status != http.StatusForbidden {
			t.Errorf("INV-T2: %s %s without plugin:manage = %d, want 403; body=%s",
				probe.method, probe.path, status, body)
		}
	}
}

// A deployment with no plugins installed must be exactly what it was before
// this WP: the dispatcher is wired into every CRUD engine, so "no plugins" has
// to cost nothing and change nothing.
func TestWritesAreUnchangedWithNoPluginsInstalled(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			status, body, rec := e.post("/api/v1/contact", map[string]any{
				"name": "No Plugins Here", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create = %d: %s", status, body)
			}
			if got, _ := rec["name"].(string); got != "No Plugins Here" {
				t.Errorf("a tenant with no plugins saw its record changed: %q", got)
			}
		})
	}
}

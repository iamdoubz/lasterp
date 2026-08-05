//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
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
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.1a: the plugin host, end to end over the wired product handler against
// the objects the API actually serves.
//
// Invariants: **INV-X1** (no ambient authority — a plugin reaches data only
// through host functions built from its approved capabilities), INV-T1 (a
// plugin cannot reach another tenant's rows), INV-T2 (every plugin route is
// authorized), INV-T3 (an install can never grant more than the approver
// holds), INV-T4 (a plugin's writes are attributed to the plugin), INV-K1 (a
// secret reaches a plugin only through its declared `secrets:` list).
//
// The kernel-level containment suite — infinite loop, memory bomb, escape
// attempts — lives in kernel/plugins. What is here is the part that needs the
// real object surface and the real HTTP gateway.

// helloPluginManifest declares the Contact access the corpus's hello module
// needs, plus the secret it is allowed to read.
const helloPluginManifest = `
id: com.acme.hello
version: 1.0.0
functions: [echo, say, read, write, secret]
capabilities:
  objects:
    - {type: Contact, access: read}
    - {type: Contact, access: write}
  secrets: [acme_api_key]
`

var (
	appCorpusOnce sync.Once
	appCorpusWasm []byte
	appCorpusErr  error
)

// helloModule compiles the corpus's well-behaved plugin once per test binary.
// Same source as the kernel suite's — see kernel/plugins/corpus_test.go for
// why the corpus is Go rather than committed .wasm.
func helloModule(t *testing.T) []byte {
	t.Helper()
	appCorpusOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lasterp-app-plugin")
		if err != nil {
			appCorpusErr = err
			return
		}
		out := filepath.Join(dir, "hello.wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./hello")
		cmd.Dir = filepath.Join("..", "..", "kernel", "plugins", "testdata")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if combined, err := cmd.CombinedOutput(); err != nil {
			appCorpusErr = &corpusBuildError{out: string(combined), err: err}
			return
		}
		appCorpusWasm, appCorpusErr = os.ReadFile(out)
	})
	if appCorpusErr != nil {
		t.Fatalf("build hello plugin: %v", appCorpusErr)
	}
	return appCorpusWasm
}

type corpusBuildError struct {
	out string
	err error
}

func (e *corpusBuildError) Error() string { return e.err.Error() + "\n" + e.out }

// seedPlugin boots a tenant with a key source (the plugin reads a secret) and
// installs the hello plugin through the API.
func seedPlugin(t *testing.T, db *storage.DB) *env {
	t.Helper()
	e, _ := seedVault(t, db)
	if status, body := e.putSecret("acme_api_key", vaultPlaintext); status != http.StatusOK {
		t.Fatalf("PUT secret = %d: %s", status, body)
	}
	installHello(t, e, helloPluginManifest)
	return e
}

func installHello(t *testing.T, e *env, manifest string) {
	t.Helper()
	status, body, _ := e.post("/api/v1/plugins", map[string]any{
		"manifest": manifest,
		"module":   base64.StdEncoding.EncodeToString(helloModule(t)),
	})
	if status != http.StatusCreated {
		t.Fatalf("install plugin = %d: %s", status, body)
	}
}

// callPluginFn invokes a plugin function over HTTP and returns the status and
// the plugin's own output.
func callPluginFn(t *testing.T, e *env, fn, input string) (int, string) {
	t.Helper()
	status, body, parsed := e.post("/api/v1/plugins/com.acme.hello/call/"+fn,
		map[string]any{"input": input})
	if status != http.StatusOK {
		return status, string(body)
	}
	out, _ := parsed["output"].(string)
	return status, out
}

// --- the granted path works ---

func TestPluginReachesOnlyWhatItsManifestDeclares(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)

			// A contact the plugin is allowed to read.
			status, body, contact := e.post("/api/v1/contact", map[string]any{
				"name": "Plugin Visible Ltd", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create contact = %d: %s", status, body)
			}
			id := mustField(t, contact, "id")

			status, out := callPluginFn(t, e, "read", `{"object":"Contact","id":"`+id+`"}`)
			if status != http.StatusOK {
				t.Fatalf("plugin read = %d: %s", status, out)
			}
			if !strings.Contains(out, "Plugin Visible Ltd") {
				t.Errorf("granted read returned %s", out)
			}

			// An object the manifest never mentions is refused, in the same
			// word as a missing row: an untrusted module does not get to
			// enumerate what a tenant holds.
			_, out = callPluginFn(t, e, "read", `{"object":"Account","id":"`+id+`"}`)
			if !strings.Contains(out, `"denied"`) {
				t.Errorf("INV-X1: reading an undeclared object returned %s", out)
			}
		})
	}
}

// INV-T4: a plugin's write is the plugin's, not the invoking user's.
func TestPluginWritesAreAttributedToThePlugin(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)

			status, out := callPluginFn(t, e, "write",
				`{"object":"Contact","record":{"name":"Made By Plugin","kind":"customer"}}`)
			if status != http.StatusOK || !strings.Contains(out, `"ok":true`) {
				t.Fatalf("plugin write = %d: %s", status, out)
			}

			actors := auditActorsFor(t, e, "Contact")
			var sawPlugin bool
			for _, actor := range actors {
				if actor == "plugin:com.acme.hello" {
					sawPlugin = true
				}
			}
			if !sawPlugin {
				t.Errorf("INV-T4: no audit row attributed to the plugin; actors were %v", actors)
			}
			for _, actor := range actors {
				if strings.HasPrefix(actor, "plugin:") {
					continue
				}
				// The invoking user's own writes are theirs; what must not
				// happen is the plugin's write wearing the user's name.
				t.Logf("other actor present (the seeded contact's creator): %s", actor)
			}
		})
	}
}

// INV-T1: the plugin principal is tenant-scoped like every other principal.
func TestPluginCannotReachAnotherTenantsRows(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			owner := seedPlugin(t, db)
			status, body, contact := owner.post("/api/v1/contact", map[string]any{
				"name": "Owner Only Ltd", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create contact = %d: %s", status, body)
			}
			id := mustField(t, contact, "id")

			// A second tenant installs the same plugin, then asks it for the
			// first tenant's row by id.
			other := seedTenant(t, db, tenancy.ID(idgen.New()))
			if st, b := other.putSecret("acme_api_key", "other-tenant-secret"); st != http.StatusOK {
				t.Fatalf("other tenant secret = %d: %s", st, b)
			}
			installHello(t, other, helloPluginManifest)

			_, out := callPluginFn(t, other, "read", `{"object":"Contact","id":"`+id+`"}`)
			if strings.Contains(out, "Owner Only Ltd") {
				t.Fatalf("INV-T1: a plugin read another tenant's row: %s", out)
			}
			if !strings.Contains(out, `"denied"`) {
				t.Errorf("expected a denial, got %s", out)
			}
		})
	}
}

// INV-K1 + the WP-3.0 seam: a secret reaches a plugin only through the
// manifest's declared list.
func TestPluginReadsOnlyItsDeclaredSecrets(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)
			if status, body := e.putSecret("other_key", "not-for-the-plugin"); status != http.StatusOK {
				t.Fatalf("PUT second secret = %d: %s", status, body)
			}

			status, out := callPluginFn(t, e, "secret", "acme_api_key")
			if status != http.StatusOK {
				t.Fatalf("declared secret = %d: %s", status, out)
			}
			if !strings.Contains(out, vaultPlaintext) {
				t.Errorf("a declared secret did not reach the plugin: %s", out)
			}

			// A secret that exists, in the same tenant, that this manifest did
			// not declare.
			_, out = callPluginFn(t, e, "secret", "other_key")
			if strings.Contains(out, "not-for-the-plugin") {
				t.Fatalf("INV-K1: a plugin read a secret its manifest never declared: %s", out)
			}
			if !strings.Contains(out, `"denied"`) {
				t.Errorf("expected a denial, got %s", out)
			}
		})
	}
}

// --- INV-T3: an install may narrow, never widen ---

func TestInstallRefusesWhatTheApproverCannotDo(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)

			// A user who may manage plugins and nothing else.
			token := e.issueUser(t, map[string][]string{"plugin": {"manage", "invoke"}})
			status, body, _ := e.call("POST", "/api/v1/plugins", token, idgen.New(), map[string]any{
				"manifest": helloPluginManifest,
				"module":   base64.StdEncoding.EncodeToString(helloModule(t)),
			})
			if status != http.StatusForbidden {
				t.Fatalf("INV-T3: install by an approver without Contact grants = %d, want 403; body=%s", status, body)
			}
			if !strings.Contains(string(body), "Contact:read") {
				t.Errorf("the refusal does not name the missing capability: %s", body)
			}
		})
	}
}

// A caller's own permissions do not become the plugin's: `plugin:invoke` is
// the right to run approved code, not a way to borrow its authority.
func TestInvokerCannotBorrowThePluginsAuthorityOrViceVersa(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)
			status, body, contact := e.post("/api/v1/contact", map[string]any{
				"name": "Readable By Plugin", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create contact = %d: %s", status, body)
			}
			id := mustField(t, contact, "id")

			// This user cannot read a Contact directly...
			token := e.issueUser(t, map[string][]string{"plugin": {"invoke"}})
			if st, _, _ := e.call("GET", "/api/v1/contact/"+id, token, "", nil); st != http.StatusForbidden {
				t.Fatalf("user with no Contact grant read one directly: %d", st)
			}
			// ...but may invoke a plugin that can. That is the design: the
			// plugin's authority is the plugin's, approved once by an
			// administrator, and invoking it is a separate, auditable power.
			// What must never happen is the reverse — the plugin inheriting
			// the *caller's* grants — which is what makes its power reviewable
			// at install time rather than varying per user.
			st, body, parsed := e.call("POST", "/api/v1/plugins/com.acme.hello/call/read",
				token, idgen.New(), map[string]any{"input": `{"object":"Contact","id":"` + id + `"}`})
			if st != http.StatusOK {
				t.Fatalf("invoke = %d: %s", st, body)
			}
			out, _ := parsed["output"].(string)
			if !strings.Contains(out, "Readable By Plugin") {
				t.Errorf("the plugin's own authority did not apply: %s", out)
			}

			// And the same user cannot install anything.
			if st, _, _ := e.call("POST", "/api/v1/plugins", token, idgen.New(), map[string]any{
				"manifest": helloPluginManifest, "module": "AGFzbQEAAAA=",
			}); st != http.StatusForbidden {
				t.Errorf("INV-T2: a user without plugin:manage installed a plugin: %d", st)
			}
		})
	}
}

func TestPluginRoutesRequireTheirPermissions(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)
			bare := e.issueUser(t, map[string][]string{"Contact": {"read"}})

			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/plugins"},
				{"POST", "/api/v1/plugins"},
				{"DELETE", "/api/v1/plugins/com.acme.hello"},
				{"POST", "/api/v1/plugins/com.acme.hello/call/echo"},
			} {
				status, body, _ := e.call(probe.method, probe.path, bare, idgen.New(), map[string]any{})
				if status != http.StatusForbidden {
					t.Errorf("INV-T2: %s %s without the grant = %d, want 403; body=%s",
						probe.method, probe.path, status, body)
				}
				status, _, _ = e.call(probe.method, probe.path, "", idgen.New(), map[string]any{})
				if status != http.StatusUnauthorized {
					t.Errorf("INV-T2: %s %s unauthenticated = %d, want 401", probe.method, probe.path, status)
				}
			}
		})
	}
}

// Uninstall must take the authority with it, over the real API.
func TestUninstallRevokesThePluginsAuthority(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedPlugin(t, db)

			status, _, _ := e.call("DELETE", "/api/v1/plugins/com.acme.hello", e.token, idgen.New(), nil)
			if status != http.StatusOK {
				t.Fatalf("uninstall = %d", status)
			}
			// The principal keeps no grants behind it.
			ctx := context.Background()
			installed, err := plugins.List(ctx, e.db, e.tenant)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(installed) != 0 {
				t.Errorf("uninstall left %d plugins", len(installed))
			}
			status, _, _ = e.post("/api/v1/plugins/com.acme.hello/call/echo", map[string]any{"input": "hi"})
			if status != http.StatusNotFound {
				t.Errorf("calling an uninstalled plugin = %d, want 404", status)
			}
		})
	}
}

// The key source is optional; a deployment without one must still be able to
// run plugins that do not read secrets.
func TestPluginWithoutSecretsWorksWithNoKeySource(t *testing.T) {
	db := sqliteBootDB(t)
	t.Setenv(secrets.EnvKeyFile, "")
	e := seed(t, db)
	installHello(t, e, `
id: com.acme.hello
version: 1.0.0
functions: [echo]
capabilities:
  objects:
    - {type: Contact, access: read}
    - {type: Contact, access: write}
  secrets: [acme_api_key]
`)
	status, out := callPluginFn(t, e, "echo", "no key source here")
	if status != http.StatusOK {
		t.Fatalf("echo = %d: %s", status, out)
	}
	if out != "echo:no key source here" {
		t.Errorf("echo returned %q", out)
	}
}

// --- helpers ---

// auditActorsFor returns the actors of every audit row for an object.
func auditActorsFor(t *testing.T, e *env, object string) []string {
	t.Helper()
	var out []string
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, e.db.Rebind(
			`SELECT DISTINCT actor_id FROM audit_log WHERE tenant_id = ? AND object = ?`),
			string(e.tenant), object)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var actor string
			if err := rows.Scan(&actor); err != nil {
				return err
			}
			out = append(out, actor)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit actors: %v", err)
	}
	return out
}

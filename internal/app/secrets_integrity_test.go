//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.0: the secrets vault, end to end over the wired product handler.
//
// Invariants: **INV-K1** (secret material is never persisted, logged, emitted
// or replicated in plaintext), INV-T1 (a tenant cannot reach another's
// secret), INV-T2 (every vault route is authorized), INV-T4 (every vault
// operation is attributable).
//
// INV-X1 — "a plugin without `secrets.get` cannot reach one" — is deliberately
// *not* claimed here: there is no plugin host until WP-3.1, and the roadmap
// says so. What is provable today is the stronger structural half:
// TestNoRouteReturnsASecret, i.e. there is nothing over HTTP for a plugin, an
// agent or a stolen session to reach.

const (
	vaultPlaintext = "sk-live-51H8xQ2eZvKYlo2C-do-not-log-me"
	vaultName      = "acme_api_key"
)

// testKeySource writes a key file, points the environment at it, and returns
// the loaded source. It must run before Handler is built — the server reads
// the key source once, at boot.
func testKeySource(t *testing.T) secrets.KeySource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lasterp.keys")
	if err := secrets.NewKeyFile(path, "test-key"); err != nil {
		t.Fatalf("NewKeyFile: %v", err)
	}
	t.Setenv(secrets.EnvKeyFile, path)
	src, err := secrets.LoadKeySource()
	if err != nil {
		t.Fatalf("LoadKeySource: %v", err)
	}
	return src
}

// seedVault is seed with a key source configured first, plus `secret:manage`
// in the session's grants.
func seedVault(t *testing.T, db *storage.DB) (*env, secrets.KeySource) {
	t.Helper()
	src := testKeySource(t)
	e := seed(t, db)
	return e, src
}

func (e *env) putSecret(name, value string) (int, []byte) {
	e.t.Helper()
	status, body, _ := e.call("PUT", "/api/v1/secrets/"+name, e.token, idgen.New(),
		map[string]any{"value": value, "description": "for the vault tests"})
	return status, body
}

// assertNoVaultPlaintext fails if s carries the known secret in raw or base64
// form. Base64 matters because the stored columns are base64 — a vault that
// leaked the value would leak it encoded, and a raw-string search alone would
// call that green.
func assertNoVaultPlaintext(t *testing.T, where string, s string) {
	t.Helper()
	if strings.Contains(s, vaultPlaintext) {
		t.Errorf("INV-K1: %s contains the secret in plaintext", where)
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte(vaultPlaintext)); strings.Contains(s, b64) {
		t.Errorf("INV-K1: %s contains the base64 of the secret", where)
	}
}

// --- AC: a secret round-trips without plaintext at rest ---

func TestSecretRoundTripsThroughTheAPI(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, src := seedVault(t, db)

			if status, body := e.putSecret(vaultName, vaultPlaintext); status != http.StatusOK {
				t.Fatalf("PUT secret = %d: %s", status, body)
			}

			// Stored as ciphertext: the row itself carries no plaintext.
			assertNoVaultPlaintext(t, "the secrets row", rawSecretRow(t, e, vaultName))

			// Readable by a server-side reader — the vault is not write-only,
			// it is HTTP-read-free (decisions §4).
			got, err := secrets.Get(context.Background(), e.db, src, e.tenant, vaultName,
				secrets.Reader{Kind: "module", ID: "vault-test"}, secrets.AllowAll)
			if err != nil {
				t.Fatalf("secrets.Get: %v", err)
			}
			if string(got) != vaultPlaintext {
				t.Error("the stored secret did not round-trip")
			}

			// The list route shows the metadata and nothing else.
			status, body, parsed := e.get("/api/v1/secrets")
			if status != http.StatusOK {
				t.Fatalf("GET /api/v1/secrets = %d: %s", status, body)
			}
			assertNoVaultPlaintext(t, "the list response", string(body))
			data, _ := parsed["data"].([]any)
			if len(data) != 1 {
				t.Fatalf("list returned %d secrets, want 1: %s", len(data), body)
			}
			row, _ := data[0].(map[string]any)
			if row["name"] != vaultName || row["key_id"] != "test-key" {
				t.Errorf("list row = %v, want the name and key id", row)
			}
			if _, present := row["value"]; present {
				t.Error("INV-K1: the list response carries a `value` field")
			}

			// Delete removes it; deleting again is a clean 404 rather than a
			// silent success.
			status, body, _ = e.call("DELETE", "/api/v1/secrets/"+vaultName, e.token, idgen.New(), nil)
			if status != http.StatusOK {
				t.Fatalf("DELETE secret = %d: %s", status, body)
			}
			status, _, _ = e.call("DELETE", "/api/v1/secrets/"+vaultName, e.token, idgen.New(), nil)
			if status != http.StatusNotFound {
				t.Errorf("deleting a missing secret = %d, want 404", status)
			}
		})
	}
}

// --- AC: a plugin without secrets.get cannot reach one ---
//
// The provable-today form, and the load-bearing test of this WP: no route
// returns secret material. A reveal endpoint added later fails here rather
// than in review.

func TestNoRouteReturnsASecret(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			if status, body := e.putSecret(vaultName, vaultPlaintext); status != http.StatusOK {
				t.Fatalf("PUT secret = %d: %s", status, body)
			}

			// The declared surface: every action under /api/v1/secrets is one of
			// the three the vault publishes, and none of them is a read of a value.
			allowed := map[string]bool{
				"GET /api/v1/secrets":           true,
				"PUT /api/v1/secrets/{name}":    true,
				"DELETE /api/v1/secrets/{name}": true,
			}
			for _, a := range allActions(t, db) {
				if !strings.HasPrefix(a.Path, "/api/v1/secrets") {
					continue
				}
				if !allowed[a.Method+" "+a.Path] {
					t.Errorf("%s %s is a vault route this test does not know about. If it "+
						"returns a secret's value it breaks INV-K1 and needs an ADR, not a "+
						"test edit (WP-3.0-decisions.md §4).", a.Method, a.Path)
				}
			}

			// The served surface: probe the shapes a reveal endpoint would take.
			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/secrets/" + vaultName},
				{"GET", "/api/v1/secrets/" + vaultName + "/value"},
				{"GET", "/api/v1/secrets/" + vaultName + "/reveal"},
				{"POST", "/api/v1/secrets/" + vaultName + "/reveal"},
				{"GET", "/api/v1/secrets?include=values"},
			} {
				status, body, _ := e.call(probe.method, probe.path, e.token, idgen.New(), nil)
				assertNoVaultPlaintext(t, probe.method+" "+probe.path, string(body))
				if status == http.StatusOK && strings.Contains(probe.path, "reveal") {
					t.Errorf("%s %s answered 200 — a reveal endpoint exists", probe.method, probe.path)
				}
			}

			// The whole read surface: every GET the server routes, swept for the
			// plaintext. Path parameters are filled with the secret's own name so
			// a route that happens to take one is asked for *this* secret.
			for _, a := range allActions(t, db) {
				if a.Method != http.MethodGet {
					continue
				}
				path := strings.ReplaceAll(concretePath(a.Path), "/x", "/"+vaultName)
				_, body, _ := e.call("GET", path, e.token, "", nil)
				assertNoVaultPlaintext(t, "GET "+path, string(body))
			}
		})
	}
}

// --- AC: a tenant cannot read another's secret (INV-T1) ---

func TestSecretsAreTenantScopedOverTheAPI(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			owner, _ := seedVault(t, db)
			if status, body := owner.putSecret(vaultName, vaultPlaintext); status != http.StatusOK {
				t.Fatalf("PUT secret = %d: %s", status, body)
			}

			// A second tenant on the same server, with the same permissions and
			// the same secret name.
			other := seedTenant(t, db, tenancy.ID(idgen.New()))
			status, body, parsed := other.get("/api/v1/secrets")
			if status != http.StatusOK {
				t.Fatalf("GET /api/v1/secrets as the other tenant = %d: %s", status, body)
			}
			assertNoVaultPlaintext(t, "the other tenant's list", string(body))
			if data, _ := parsed["data"].([]any); len(data) != 0 {
				t.Errorf("INV-T1: the other tenant sees %d secrets, want 0", len(data))
			}

			// Deleting by name across the boundary is a 404, and leaves the
			// owner's secret alone.
			status, _, _ = other.call("DELETE", "/api/v1/secrets/"+vaultName, other.token, idgen.New(), nil)
			if status != http.StatusNotFound {
				t.Errorf("INV-T1: cross-tenant delete = %d, want 404", status)
			}
			_, _, ownerList := owner.get("/api/v1/secrets")
			if data, _ := ownerList["data"].([]any); len(data) != 1 {
				t.Error("INV-T1: the owner's secret did not survive another tenant's delete")
			}
		})
	}
}

// --- AC: reading a secret is authorized and audited (INV-T2/T4) ---

func TestVaultRoutesRequireThePermission(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)

			// A principal with every other grant in the product and no
			// `secret:manage`. The vault is its own power, not a consequence of
			// being an administrator of something else.
			grants := fullGrants()
			delete(grants, "secret")
			token := e.issueUser(t, grants)

			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/secrets"},
				{"PUT", "/api/v1/secrets/" + vaultName},
				{"DELETE", "/api/v1/secrets/" + vaultName},
			} {
				status, body, _ := e.call(probe.method, probe.path, token, idgen.New(),
					map[string]any{"value": vaultPlaintext})
				if status != http.StatusForbidden {
					t.Errorf("INV-T2: %s %s without secret:manage = %d, want 403; body=%s",
						probe.method, probe.path, status, body)
				}
			}

			// And unauthenticated is unauthenticated.
			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/secrets"},
				{"PUT", "/api/v1/secrets/" + vaultName},
			} {
				status, _, _ := e.call(probe.method, probe.path, "", idgen.New(),
					map[string]any{"value": vaultPlaintext})
				if status != http.StatusUnauthorized {
					t.Errorf("INV-T2: %s %s with no session = %d, want 401", probe.method, probe.path, status)
				}
			}
		})
	}
}

func TestVaultWritesAreAttributableToTheirActor(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, src := seedVault(t, db)
			if status, body := e.putSecret(vaultName, vaultPlaintext); status != http.StatusOK {
				t.Fatalf("PUT secret = %d: %s", status, body)
			}
			if _, err := secrets.Get(context.Background(), e.db, src, e.tenant, vaultName,
				secrets.Reader{Kind: "plugin", ID: "com.acme.x"}, secrets.AllowAll); err != nil {
				t.Fatalf("secrets.Get: %v", err)
			}

			rows := vaultAudit(t, e)
			if len(rows) != 2 {
				t.Fatalf("INV-T4: %d audit rows for the vault, want create + read: %v", len(rows), rows)
			}
			for _, r := range rows {
				if r["actor"] == "" {
					t.Errorf("INV-T4: audit row %v has no actor", r)
				}
				assertNoVaultPlaintext(t, "an audit row", r["changes"])
			}
			if rows[1]["action"] != "read" || !strings.Contains(rows[1]["changes"], "com.acme.x") {
				t.Errorf("a read must record who read it: %v", rows[1])
			}
		})
	}
}

// --- AC: grep the logs, the event store and an export for the plaintext ---

func TestAStoredSecretAppearsNowhereDownstream(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			if status, body := e.putSecret(vaultName, vaultPlaintext); status != http.StatusOK {
				t.Fatalf("PUT secret = %d: %s", status, body)
			}

			// A contact, so every probe below has something to find. A feed or a
			// snapshot that is simply empty would pass this test while proving
			// nothing.
			marker := "Canary Industries " + idgen.New()
			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": marker, "kind": "customer",
			}); status != http.StatusCreated {
				t.Fatalf("create contact = %d: %s", status, body)
			}

			probes := map[string]string{
				"the audit log":   dumpTable(t, e, "audit_log"),
				"the event store": dumpTable(t, e, "events"),
				"the change feed": dumpTable(t, e, "change_feed"),
			}
			for _, path := range []string{
				"/api/v1/sync/changes?include=rows&limit=1000",
				"/api/v1/sync/snapshot?object=Contact&limit=100",
				"/api/v1/reports/trial_balance/export?currency=EUR&format=csv",
			} {
				_, body, _ := e.get(path)
				probes["GET "+path] = string(body)
			}

			for where, blob := range probes {
				assertNoVaultPlaintext(t, where, blob)
			}

			// Non-vacuous: the feed and the snapshot really were carrying data
			// while the secret was not in them.
			if !strings.Contains(probes["GET /api/v1/sync/changes?include=rows&limit=1000"], marker) {
				t.Error("the change feed probe found no rows at all — it proves nothing")
			}
			if !strings.Contains(probes["GET /api/v1/sync/snapshot?object=Contact&limit=100"], marker) {
				t.Error("the snapshot probe found no rows at all — it proves nothing")
			}
			if !strings.Contains(probes["the change feed"], "Contact") {
				t.Error("the change_feed table probe found nothing — it proves nothing")
			}

			// And the vault is not a replicable object: nothing in the sync
			// surface knows the word at all, which is what keeps a secret off
			// every client replica structurally rather than by a filter
			// (decisions §6).
			_, body, _ := e.get("/api/v1/sync/snapshot?object=Secret&limit=100")
			if strings.Contains(string(body), "wrapped_dek") {
				t.Error("INV-K1: the vault is exposed as a replicable object")
			}
			_, body, parsed := e.get("/api/v1/meta/objects")
			assertNoVaultPlaintext(t, "the metadata object list", string(body))
			metaObjects, _ := parsed["data"].([]any)
			for _, o := range metaObjects {
				if obj, _ := o.(map[string]any); obj["name"] == "Secret" {
					t.Error("INV-K1: the vault is registered as a metadata object and would " +
						"be replicated to every client")
				}
			}
		})
	}
}

// A deployment with no key file must say so rather than fail obscurely — and
// must not invent a key of its own (decisions §3).
func TestVaultWithoutAKeySourceRefusesWritesAndSaysWhy(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			t.Setenv(secrets.EnvKeyFile, "")
			e := seed(t, db)

			status, body, parsed := e.call("PUT", "/api/v1/secrets/"+vaultName, e.token, idgen.New(),
				map[string]any{"value": vaultPlaintext})
			if status != http.StatusServiceUnavailable {
				t.Fatalf("PUT with no key source = %d, want 503; body=%s", status, body)
			}
			if got, _ := parsed["type"].(string); got != "secrets-no-key-source" {
				t.Errorf("problem type = %q, want secrets-no-key-source", got)
			}
			if !strings.Contains(string(body), secrets.EnvKeyFile) {
				t.Error("the refusal does not name the environment variable to set")
			}

			// Listing still works: an operator who lost the key file can still
			// see what they lost.
			if status, _, _ := e.get("/api/v1/secrets"); status != http.StatusOK {
				t.Errorf("GET /api/v1/secrets with no key source = %d, want 200", status)
			}
		})
	}
}

// --- helpers ---

func rawSecretRow(t *testing.T, e *env, name string) string {
	t.Helper()
	var keyID, wrapped, ciphertext string
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, e.db.Rebind(
			`SELECT key_id, wrapped_dek, ciphertext FROM secrets WHERE tenant_id = ? AND name = ?`),
			string(e.tenant), name).Scan(&keyID, &wrapped, &ciphertext)
	})
	if err != nil {
		t.Fatalf("read secrets row: %v", err)
	}
	return fmt.Sprintf("key_id=%s wrapped_dek=%s ciphertext=%s", keyID, wrapped, ciphertext)
}

func vaultAudit(t *testing.T, e *env) []map[string]string {
	t.Helper()
	var out []map[string]string
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, e.db.Rebind(
			`SELECT action, actor_id, changes FROM audit_log
			 WHERE tenant_id = ? AND object = 'secret' ORDER BY at, id`), string(e.tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []map[string]string
		for rows.Next() {
			var action, actor, changes string
			if err := rows.Scan(&action, &actor, &changes); err != nil {
				return err
			}
			list = append(list, map[string]string{"action": action, "actor": actor, "changes": changes})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		t.Fatalf("read vault audit rows: %v", err)
	}
	return out
}

// dumpTable returns every row of a tenant-scoped table as one string, for the
// plaintext sweep. Reading it raw rather than through an API is the point: the
// question is what is *stored*, not what is served.
func dumpTable(t *testing.T, e *env, table string) string {
	t.Helper()
	var b strings.Builder
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		// #nosec G202 -- table is a literal from this test's own call sites.
		rows, err := tx.QueryContext(ctx, e.db.Rebind(`SELECT * FROM `+table+` WHERE tenant_id = ?`), string(e.tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.RawBytes)
			}
			if err := rows.Scan(cells...); err != nil {
				return err
			}
			for _, c := range cells {
				b.Write(*c.(*sql.RawBytes))
				b.WriteByte(' ')
			}
			b.WriteByte('\n')
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	return b.String()
}

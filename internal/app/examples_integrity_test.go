//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/plugins"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.2b's acceptance criteria, executed rather than described:
//
//   - "afternoon-plugin tutorial completes" — TestAfternoonTutorialCompletes
//     runs the exact sequence docs/23-PLUGIN-TUTORIAL.md tells an author to
//     run: scaffold, build, keygen, pack, install, call, uninstall. A tutorial
//     nothing executes is a tutorial that rots.
//   - "example plugins pass" — the two examples in examples/plugins/ are built
//     from source, installed through the signed-bundle path, and driven end to
//     end against the wired product handler.
//
// Invariants: INV-T3/T4 through the bundle install path (the approver still
// bounds the manifest, the audit row still names the person), INV-K1 for the
// notifier's credential, and INV-X1 for both examples' capability sets.

// buildWasm compiles a plugin module from a directory that is its own Go
// module, the way the corpus and every real plugin are built.
func buildWasm(t *testing.T, dir string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plugin.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s:\n%s\n%v", dir, combined, err)
	}
	module, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read module: %v", err)
	}
	return module
}

// trustedPublisher writes a signing key and points this deployment's trust file
// at it. It must run **before** the handler is built: the trust store is read
// once, at boot, like the vault's key source.
func trustedPublisher(t *testing.T) plugins.SigningKey {
	t.Helper()
	dir := t.TempDir()
	key, err := plugins.NewSigningKey(filepath.Join(dir, "publisher.key"), "example-publisher")
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	trustFile := filepath.Join(dir, "trust")
	if err := os.WriteFile(trustFile, []byte(key.PublicLine()+"\n"), 0o600); err != nil {
		t.Fatalf("write trust file: %v", err)
	}
	t.Setenv(plugins.EnvTrustFile, trustFile)
	return key
}

// installBundleOverHTTP packs and installs through the API the CLI uses.
func installBundleOverHTTP(t *testing.T, e *env, key plugins.SigningKey, manifest string, module []byte) map[string]any {
	t.Helper()
	bundle, err := plugins.Pack([]byte(manifest), module, nil, key.ID, key.Key)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	status, body, parsed := e.post("/api/v1/plugins/bundle", map[string]any{
		"bundle": base64.StdEncoding.EncodeToString(bundle),
	})
	if status != http.StatusCreated {
		t.Fatalf("install bundle = %d: %s", status, body)
	}
	return parsed
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestAfternoonTutorialCompletes is AC-1, executed.
func TestAfternoonTutorialCompletes(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			// 1. `lasterp plugin new --lang go -id com.acme.afternoon`
			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}

			// 2. build it, 3. pack it under the publisher key, 4. install it —
			// through the same API `lasterp plugin install` posts to.
			module := buildWasm(t, dir)
			installed := installBundleOverHTTP(t, e, key, readFile(t, filepath.Join(dir, "manifest.yaml")), module)
			if installed["id"] != "com.acme.afternoon" {
				t.Fatalf("installed %v", installed)
			}

			// 5. call the route the scaffold declares and serves. This is the
			// whole promise: an author who typed four commands has a working
			// endpoint on their own ERP.
			resp := e.raw(t, "GET", "/ext/com.acme.afternoon/hello", e.token, "", nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET the scaffold's route = %d", resp.StatusCode)
			}
			var greeting map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&greeting); err != nil {
				t.Fatalf("decode greeting: %v", err)
			}
			if greeting["hello"] == "" || greeting["hello"] == nil {
				t.Fatalf("the scaffold's endpoint answered %v", greeting)
			}

			// 6. uninstall, because a tutorial that leaves the tenant dirty has
			// not shown the whole lifecycle.
			status, body, _ := e.call("DELETE", "/api/v1/plugins/com.acme.afternoon", e.token, idgen.New(), nil)
			if status != http.StatusOK {
				t.Fatalf("uninstall = %d: %s", status, body)
			}
			after := e.raw(t, "GET", "/ext/com.acme.afternoon/hello", e.token, "", nil)
			defer func() { _ = after.Body.Close() }()
			if after.StatusCode != http.StatusNotFound {
				t.Fatalf("the route still answers after uninstall: %d", after.StatusCode)
			}
		})
	}
}

// TestCommissionCalcExample is half of AC-2: a plugin that reacts to business
// events, keeps its own state, and answers a question about it.
func TestCommissionCalcExample(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			key := trustedPublisher(t)
			e, keys := seedVault(t, db)

			dir := filepath.Join("..", "..", "examples", "plugins", "commission-calc")
			installBundleOverHTTP(t, e, key, readFile(t, filepath.Join(dir, "manifest.yaml")), buildWasm(t, dir))

			// A posted invoice, through the real invoicing pipeline.
			netMinor := seedPostedInvoice(t, e)

			// One delivery pass, driven rather than waited for.
			objects, err := crudObjects()
			if err != nil {
				t.Fatalf("crudObjects: %v", err)
			}
			runner := plugins.NewRunner(pluginHost(db, objects, keys), nil)
			if _, err := runner.Deliver(ctx, e.tenant); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			resp := e.raw(t, "GET", "/ext/com.lasterp.commission-calc/report?currency=USD", e.token, "", nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("report = %d", resp.StatusCode)
			}
			var report struct {
				Currency        string  `json:"currency"`
				CommissionMinor float64 `json:"commission_minor"`
				BasisPoints     float64 `json:"basis_points"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			want := netMinor * int64(report.BasisPoints) / 10000
			if want == 0 {
				t.Fatal("the seeded invoice has no net amount — the assertion below would pass on nothing")
			}
			if int64(report.CommissionMinor) != want {
				t.Fatalf("commission = %v minor units, want %d (5%% of %d)", report.CommissionMinor, want, netMinor)
			}

			// At-least-once delivery means the hook must be safe to run twice.
			// A second pass over the same feed position must not double the
			// total — which is the property its kv dedupe key exists for.
			if _, err := runner.Deliver(ctx, e.tenant); err != nil {
				t.Fatalf("second Deliver: %v", err)
			}
			again := e.raw(t, "GET", "/ext/com.lasterp.commission-calc/report?currency=USD", e.token, "", nil)
			defer func() { _ = again.Body.Close() }()
			var second struct {
				CommissionMinor float64 `json:"commission_minor"`
			}
			if err := json.NewDecoder(again.Body).Decode(&second); err != nil {
				t.Fatalf("decode second report: %v", err)
			}
			if int64(second.CommissionMinor) != want {
				t.Fatalf("a redelivery changed the total to %v — the example is not idempotent", second.CommissionMinor)
			}
		})
	}
}

// TestSlackNotifierExample is the other half of AC-2: a plugin that talks to
// the outside world, with its credential in the vault and its destination in
// the manifest.
//
// It runs against a local TLS server rather than Slack, so the manifest under
// test names that server's host. The *module* is the example's own, unmodified
// — which is the part the AC is about.
func TestSlackNotifierExample(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var posted atomic.Int32
			var sawContact atomic.Bool
			slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posted.Add(1)
				body := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(body)
				if strings.Contains(string(body), "Ada Lovelace") {
					sawContact.Store(true)
				}
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(slack.Close)

			// The deployment's two outbound dials, both set before boot: this
			// server is on loopback, and it presents its own certificate.
			t.Setenv("LASTERP_PLUGIN_HTTP_ALLOW_PRIVATE", "1")
			t.Setenv("LASTERP_PLUGIN_HTTP_CA_FILE", writeCAFile(t, slack))
			key := trustedPublisher(t)
			e, keys := seedVault(t, db)

			host := strings.TrimPrefix(slack.URL, "https://")
			dir := filepath.Join("..", "..", "examples", "plugins", "slack-notifier")
			manifest := strings.Replace(readFile(t, filepath.Join(dir, "manifest.yaml")),
				"hooks.slack.com", host, 1)
			installBundleOverHTTP(t, e, key, manifest, buildWasm(t, dir))

			// The webhook URL is a credential, so it lives in the vault. Its
			// path is the secret half — which is why the audit row records only
			// the first path segment.
			webhook := slack.URL + "/services/T000/B000/zzz-secret-zzz"
			if status, body := e.putSecret("slack_webhook_url", webhook); status != http.StatusCreated && status != http.StatusOK {
				t.Fatalf("store webhook secret = %d: %s", status, body)
			}

			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "Ada Lovelace", "kind": "customer",
			}); status != http.StatusCreated {
				t.Fatalf("create contact = %d: %s", status, body)
			}

			objects, err := crudObjects()
			if err != nil {
				t.Fatalf("crudObjects: %v", err)
			}
			runner := plugins.NewRunner(pluginHost(db, objects, keys), nil)
			if _, err := runner.Deliver(ctx, e.tenant); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if posted.Load() != 1 || !sawContact.Load() {
				t.Fatalf("the notifier posted %d times, saw the contact: %v", posted.Load(), sawContact.Load())
			}

			// Delivered twice, notified once: async delivery is at-least-once
			// and the example dedupes.
			if _, err := runner.Deliver(ctx, e.tenant); err != nil {
				t.Fatalf("second Deliver: %v", err)
			}
			if posted.Load() != 1 {
				t.Fatalf("a redelivery notified again (%d posts) — the example is not idempotent", posted.Load())
			}

			// INV-K1 through the whole example: the webhook credential reached
			// Slack and nothing else. The audit row proves the call happened
			// without becoming a copy of the URL that made it.
			assertNoSecretInAudit(t, db, e, webhook, "zzz-secret-zzz")
		})
	}
}

// writeCAFile writes a test server's certificate as PEM, the way an operator
// names their internal CA.
func writeCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return path
}

// assertNoSecretInAudit sweeps the tenant's audit trail for a credential, raw
// and base64 — the shape WP-3.0's sweep established.
func assertNoSecretInAudit(t *testing.T, db *storage.DB, e *env, needles ...string) {
	t.Helper()
	// Read inside tenant context: on Postgres the audit table is under RLS, so
	// a bare query returns nothing and the sweep would report green having read
	// no rows at all.
	sawOutbound := false
	err := tenancy.WithTenant(context.Background(), db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT action, changes FROM audit_log WHERE tenant_id = ?`), string(e.tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var action, changes string
			if err := rows.Scan(&action, &changes); err != nil {
				return err
			}
			if action == "http.request" {
				sawOutbound = true
			}
			for _, needle := range needles {
				if strings.Contains(changes, needle) ||
					strings.Contains(changes, base64.StdEncoding.EncodeToString([]byte(needle))) {
					t.Fatalf("audit row (%s) leaked %q: %s", action, needle, changes)
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !sawOutbound {
		t.Fatal("no outbound call was audited — the sweep above ran on nothing")
	}
}

// seedPostedInvoice drives the real invoicing pipeline — chart of accounts,
// contact, period, tax rate, draft, post — and returns the invoice's net amount
// in minor units, which is what the commission is a percentage of.
func seedPostedInvoice(t *testing.T, e *env) int64 {
	t.Helper()
	arID := e.createAccount("1100", "Accounts Receivable", "asset")
	revID := e.createAccount("4000", "Sales Revenue", "income")
	taxID := e.createAccount("2200", "Tax Payable", "liability")

	status, body, contact := e.post("/api/v1/contact", map[string]any{
		"name": "Acme Co", "email": "ap@acme.example", "kind": "customer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create contact = %d: %s", status, body)
	}
	if status, body, _ = e.post("/api/v1/periods", map[string]any{
		"code": "2026-07", "start_date": "2026-07-01", "end_date": "2026-07-31",
	}); status != http.StatusCreated {
		t.Fatalf("create period = %d: %s", status, body)
	}
	if status, body, _ = e.post("/api/v1/taxrates", map[string]any{
		"jurisdiction": "US-CA", "category": "sales", "rate": "0.10",
		"rounding": "half_even", "as_of": "2026-01-01", "name": "CA sales",
	}); status != http.StatusCreated {
		t.Fatalf("create tax rate = %d: %s", status, body)
	}

	status, body, draft := e.post("/api/v1/invoices", map[string]any{
		"contact_id": mustField(t, contact, "id"), "currency": "USD", "issue_date": "2026-07-15",
		"ar_account": arID, "tax_account": taxID,
		"lines": []map[string]any{{
			"description": "Consulting", "quantity": 1, "unit_price_minor": 10000,
			"revenue_account": revID, "tax_jurisdiction": "US-CA", "tax_category": "sales",
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create draft = %d: %s", status, body)
	}
	id := mustField(t, draft, "ID")
	status, body, posted := e.call("POST", "/api/v1/invoices/"+id+"/post", e.token, idgen.New(),
		map[string]any{"period": "2026-07"})
	if status != http.StatusOK {
		t.Fatalf("post invoice = %d: %s", status, body)
	}
	net, ok := posted["NetMinor"].(float64)
	if !ok {
		t.Fatalf("posted invoice has no NetMinor: %v", posted)
	}
	return int64(net)
}

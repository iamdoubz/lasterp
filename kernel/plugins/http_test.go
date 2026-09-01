// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.2a, the outbound half. Invariants: **INV-X1** (a plugin reaches the
// network only through a capability-checked host function, and only where its
// manifest says), INV-T4 (every call is attributable), **INV-K1** (a secret a
// plugin carries out never lands in the audit log).
//
// The adversary these tests run is `testdata/web`, compiled by the same
// toolchain as the rest of the corpus.

// webManifest declares everything the web module imports — an unsatisfied
// import means the module cannot instantiate at all, which is INV-X1's
// mechanism and would make every test here a false negative.
func webManifest(httpBlock string) string {
	return `
id: com.acme.web
version: 1.0.0
functions: [fetch, exfiltrate, report, naughty, write]
capabilities:
  objects:
    - {type: Widget, access: read}
    - {type: Widget, access: write}
  secrets: [acme_api_key]
` + httpBlock
}

// httpTo is the allowlist block for one destination.
func httpTo(hostPort string, methods ...string) string {
	if len(methods) == 0 {
		methods = []string{"GET", "POST"}
	}
	return "  http:\n    - {host: " + hostPort + ", methods: [" + strings.Join(methods, ", ") + "]}\n"
}

// outboundHost is a Host wired for one policy.
func outboundHost(db *storage.DB, policy HTTPPolicy) Host {
	return Host{DB: db, Limits: DefaultLimits, HTTP: policy}
}

// tlsServer starts an HTTPS test server and returns it with the policy that
// trusts it. Private networks are permitted in that policy because a test
// server is on loopback — the *point* of the guard — so every test that needs a
// successful call says so explicitly, and the tests that check the guard use
// the strict policy instead.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, HTTPPolicy) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, HTTPPolicy{AllowPrivateNetworks: true, RootCAs: pool}
}

// hostPort is the server's host:port, which is also its allowlist entry.
func hostPort(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "https://")
}

// fetchReply is the host's JSON answer, as the plugin returns it.
type fetchReply struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Result struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"result"`
}

// fetch runs the web plugin's fetch export and parses the host's reply.
func fetch(t *testing.T, h Host, tenant tenancy.ID, p *Installed, req map[string]any) fetchReply {
	t.Helper()
	in, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal fetch request: %v", err)
	}
	out, err := Call(context.Background(), h, tenant, p, "fetch", in)
	if err != nil {
		t.Fatalf("Call fetch: %v", err)
	}
	var reply fetchReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("fetch reply is not JSON (%s): %v", out, err)
	}
	return reply
}

func TestOutboundCallReachesAnAllowlistedHost(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			var gotAuth, gotBody string
			var hits atomic.Int32
			srv, policy := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				gotAuth = r.Header.Get("X-Api-Key")
				b := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(b)
				gotBody = string(b)
				w.WriteHeader(http.StatusTeapot)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), helloApprover(t, db, tenant))

			reply := fetch(t, outboundHost(db, policy), tenant, p, map[string]any{
				"method":  "POST",
				"url":     srv.URL + "/hook",
				"headers": map[string]any{"X-Api-Key": "k1"},
				"body":    `{"hello":"world"}`,
			})
			if !reply.OK {
				t.Fatalf("allowlisted call was refused: %s", reply.Error)
			}
			if reply.Result.Status != http.StatusTeapot || reply.Result.Body != `{"ok":true}` {
				t.Fatalf("plugin saw status %d body %q", reply.Result.Status, reply.Result.Body)
			}
			if hits.Load() != 1 || gotAuth != "k1" || gotBody != `{"hello":"world"}` {
				t.Fatalf("server saw hits=%d auth=%q body=%q", hits.Load(), gotAuth, gotBody)
			}
		})
	}
}

func TestOutboundRefusesWhatTheManifestDidNotDeclare(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			srv, policy := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			other, _ := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))

			tenant := newTenant(t, db)
			// Declares the first host, GET only.
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv), "GET")), corpusModule(t, "web"), helloApprover(t, db, tenant))
			h := outboundHost(db, policy)

			for what, req := range map[string]map[string]any{
				"undeclared host":   {"method": "GET", "url": other.URL + "/x"},
				"undeclared method": {"method": "POST", "url": srv.URL + "/x"},
				"plaintext":         {"method": "GET", "url": "http://" + hostPort(srv) + "/x"},
				"credentials":       {"method": "GET", "url": "https://user:pw@" + hostPort(srv) + "/x"},
				"not a url":         {"method": "GET", "url": "::::"},
			} {
				reply := fetch(t, h, tenant, p, req)
				if reply.OK {
					t.Fatalf("%s: the call was allowed", what)
				}
				if reply.Error != "denied" {
					t.Fatalf("%s: plugin was told %q, want denied", what, reply.Error)
				}
			}
			if hits.Load() != 0 {
				t.Fatalf("a refused call still reached a server %d times", hits.Load())
			}

			// Non-vacuity: the same plugin, allowed method, does reach it.
			if reply := fetch(t, h, tenant, p, map[string]any{"method": "GET", "url": srv.URL + "/x"}); !reply.OK {
				t.Fatalf("the declared call was refused too: %s — the suite proves nothing", reply.Error)
			}
		})
	}
}

// TestAllowlistIsEnforcedOnTheResolvedAddress is the SSRF property: an
// allowlist entry is a *name*, and a name resolves to whatever its owner
// chooses — including 127.0.0.1 and the cloud metadata service. The guard runs
// on the address being dialled, so the manifest's approval is not enough.
func TestAllowlistIsEnforcedOnTheResolvedAddress(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			srv, permissive := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = w.Write([]byte(`ok`))
			}))
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), helloApprover(t, db, tenant))

			// The deployment default: private destinations are refused even
			// though this plugin's manifest names this exact host and an
			// administrator approved it.
			strict := permissive
			strict.AllowPrivateNetworks = false
			if reply := fetch(t, outboundHost(db, strict), tenant, p, map[string]any{"url": srv.URL + "/x"}); reply.OK {
				t.Fatal("a loopback destination was dialled under the default policy")
			}
			if hits.Load() != 0 {
				t.Fatalf("the blocked dial still reached the server %d times", hits.Load())
			}

			// Non-vacuity, and the operator's dial: the same call succeeds when
			// the deployment says plugins may reach its private network. If
			// this half fails, the half above proves only that the test server
			// was unreachable.
			if reply := fetch(t, outboundHost(db, permissive), tenant, p, map[string]any{"url": srv.URL + "/x"}); !reply.OK {
				t.Fatalf("AllowPrivateNetworks did not permit the call: %s", reply.Error)
			}
			if hits.Load() != 1 {
				t.Fatalf("server hits = %d, want 1", hits.Load())
			}
		})
	}
}

func TestNonPublicAddressesAreRefused(t *testing.T) {
	guard := dialGuard(false)
	for _, addr := range []string{
		"127.0.0.1:443",          // loopback
		"10.1.2.3:443",           // RFC1918
		"192.168.0.5:443",        // RFC1918
		"169.254.169.254:443",    // the cloud metadata service
		"100.64.7.7:443",         // carrier-grade NAT
		"192.0.0.170:443",        // IETF protocol assignments
		"198.18.0.1:443",         // benchmarking
		"[::1]:443",              // IPv6 loopback
		"[fd00::1]:443",          // IPv6 unique-local
		"[fe80::1]:443",          // IPv6 link-local
		"[::ffff:127.0.0.1]:443", // IPv4-mapped loopback
		"[64:ff9b::7f00:1]:443",  // NAT64-embedded loopback
		"[64:ff9b::a01:203]:443", // NAT64-embedded RFC1918
	} {
		if err := guard("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	// Non-vacuity: a public address is not refused, or the guard is simply
	// "no".
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"} {
		if err := guard("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
}

// TestRedirectIsNotFollowed closes the one-hop allowlist bypass: the plugin
// gets the 3xx and may re-request inside its own allowlist, but the host never
// dials the destination the redirect names.
func TestRedirectIsNotFollowed(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			var elsewhereHits atomic.Int32
			elsewhere, _ := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhereHits.Add(1) }))
			srv, policy := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Redirect(w, &http.Request{}, elsewhere.URL+"/secrets", http.StatusFound)
			}))
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), helloApprover(t, db, tenant))

			reply := fetch(t, outboundHost(db, policy), tenant, p, map[string]any{"url": srv.URL + "/go"})
			if !reply.OK || reply.Result.Status != http.StatusFound {
				t.Fatalf("plugin saw ok=%v status=%d, want the 302 itself", reply.OK, reply.Result.Status)
			}
			if elsewhereHits.Load() != 0 {
				t.Fatalf("the redirect target was dialled %d times", elsewhereHits.Load())
			}
		})
	}
}

func TestOversizeResponseIsRefused(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			srv, policy := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(make([]byte, MaxHTTPResponseBytes+1024))
			}))
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), helloApprover(t, db, tenant))

			if reply := fetch(t, outboundHost(db, policy), tenant, p, map[string]any{"url": srv.URL + "/big"}); reply.OK {
				t.Fatalf("a %d-byte response was accepted", len(reply.Result.Body))
			}
		})
	}
}

// TestEveryOutboundCallIsAudited is INV-T4 for the network: the row names the
// plugin, the method and the destination — and nothing else.
func TestEveryOutboundCallIsAudited(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			srv, policy := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), helloApprover(t, db, tenant))
			h := outboundHost(db, policy)

			for i := 0; i < 3; i++ {
				if reply := fetch(t, h, tenant, p, map[string]any{
					"url": srv.URL + "/v1/send?api_key=SUPERSECRET",
				}); !reply.OK {
					t.Fatalf("call %d refused: %s", i, reply.Error)
				}
			}

			rows := auditRows(t, db, tenant, "http.request")
			if len(rows) != 3 {
				t.Fatalf("audit rows = %d, want 3 (one per call)", len(rows))
			}
			for _, row := range rows {
				if row.actor != p.Principal() {
					t.Fatalf("audit actor = %q, want %q", row.actor, p.Principal())
				}
				for _, want := range []string{`"method":"GET"`, `"host":"` + hostPort(srv) + `"`, `"path":"/v1/`} {
					if !strings.Contains(row.changes, want) {
						t.Fatalf("audit row %s does not record %s", row.changes, want)
					}
				}
				// The query string carries API keys. It is not recorded — and
				// neither is the rest of the path, because for a whole class of
				// webhook APIs the path *is* the credential.
				if strings.Contains(row.changes, "SUPERSECRET") {
					t.Fatalf("the audit row copied the query string: %s", row.changes)
				}
				if strings.Contains(row.changes, "/v1/send") {
					t.Fatalf("the audit row recorded the full path: %s", row.changes)
				}
			}
		})
	}
}

// TestOutboundCallCarryingASecretDoesNotLogIt is INV-K1 at the network edge: a
// plugin granted both `secrets:` and `http:` is doing exactly what those
// capabilities are for, and the audit trail of that call must not become the
// longest-lived copy of the credential.
func TestOutboundCallCarryingASecretDoesNotLogIt(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			var seen atomic.Bool
			srv, policy := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Sent two ways on purpose: as a bearer token, and as a path
				// segment, which is how every "unguessable webhook URL" API
				// works. Both must be absent from the audit trail.
				if strings.Contains(r.Header.Get("Authorization"), "hunter2-the-token") ||
					strings.Contains(r.URL.Path, "hunter2-the-token") {
					seen.Store(true)
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			tenant := newTenant(t, db)
			keys := testKeys(t)
			act := helloApprover(t, db, tenant)
			if err := secrets.Put(context.Background(), db, keys, tenant, "acme_api_key",
				"for the outbound test", []byte("hunter2-the-token"), string(act.UserID)); err != nil {
				t.Fatalf("seed secret: %v", err)
			}
			p := install(t, db, tenant, webManifest(httpTo(hostPort(srv))), corpusModule(t, "web"), act)

			h := outboundHost(db, policy)
			h.Keys = keys
			in, _ := json.Marshal(map[string]any{"secret": "acme_api_key", "url": srv.URL + "/services/hunter2-the-token"})
			if _, err := Call(context.Background(), h, tenant, p, "exfiltrate", in); err != nil {
				t.Fatalf("Call exfiltrate: %v", err)
			}
			if !seen.Load() {
				t.Fatal("the plugin never sent the secret — the sweep below would prove nothing")
			}

			// Raw and base64, for the reason WP-3.0's sweep gives: a column
			// that stores base64 makes a raw-only search report green.
			encoded := base64.StdEncoding.EncodeToString([]byte("hunter2-the-token"))
			for _, row := range auditRows(t, db, tenant, "") {
				if strings.Contains(row.changes, "hunter2-the-token") || strings.Contains(row.changes, encoded) {
					t.Fatalf("audit row leaked the secret: %s", row.changes)
				}
			}
		})
	}
}

// TestOutboundIsAbsentWithoutTheCapability is INV-X1's mechanism applied to the
// network: no `http:` block means no host function, and a module that imports
// one cannot be instantiated at all.
func TestOutboundIsAbsentWithoutTheCapability(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := install(t, db, tenant, webManifest(""), corpusModule(t, "web"), helloApprover(t, db, tenant))

			_, err := Call(context.Background(), outboundHost(db, HTTPPolicy{}), tenant, p, "fetch", []byte(`{"url":"https://example.com/"}`))
			if err == nil {
				t.Fatal("a plugin with no http capability still ran its fetch function")
			}
			if !strings.Contains(err.Error(), "instantiate") {
				t.Fatalf("refused at call time rather than at instantiation: %v", err)
			}
		})
	}
}

// --- audit helpers ---

type auditRow struct {
	action  string
	object  string
	actor   string
	changes string
}

// auditRows reads the tenant's audit trail, optionally filtered by action.
func auditRows(t *testing.T, db *storage.DB, tenant tenancy.ID, action string) []auditRow {
	t.Helper()
	var out []auditRow
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *txType) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT action, object, actor_id, changes FROM audit_log WHERE tenant_id = ? ORDER BY at`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []auditRow
		for rows.Next() {
			var r auditRow
			if err := rows.Scan(&r.action, &r.object, &r.actor, &r.changes); err != nil {
				return err
			}
			if action == "" || r.action == action {
				list = append(list, r)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	return out
}

// testKeys is a throwaway key file, so a plugin granted `secrets:` has a vault
// to read from.
func testKeys(t *testing.T) secrets.KeySource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lasterp.keys")
	if err := secrets.NewKeyFile(path, "plugin-test-key"); err != nil {
		t.Fatalf("NewKeyFile: %v", err)
	}
	t.Setenv(secrets.EnvKeyFile, path)
	src, err := secrets.LoadKeySource()
	if err != nil {
		t.Fatalf("LoadKeySource: %v", err)
	}
	return src
}

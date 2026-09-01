//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/automations"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/outbound"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.3c's e2e. Invariants: **INV-T4** (every outbound call is attributed to
// `automation:<id>` and audited exactly once, before it is dialled),
// **INV-K1** (the destination's URL is a credential and does not reach the
// audit log, the automation document, or any route), **INV-X1**'s dialer guard
// now covering a non-plugin caller, and **INV-T3** at the route gate.
//
// The AC is three clauses and each gets its own test below, every refusal
// paired with the mirrored success — a suite where nothing can call out reports
// green on all three for the wrong reason.

// theWebhookSecret is the whole URL, and its *path* is the credential: this is
// the shape of every Slack-style "unguessable URL" integration (WP-3.2b), which
// is why the row stores only the host and the sweep below looks for the rest.
const webhookPathSecret = "hunter2-the-token"

// webhookServer starts an HTTPS receiver, returning it, a hit counter and the
// policy that trusts it.
//
// The policy permits private networks because a test server is on loopback —
// which is the *point* of the guard — so every test that needs a successful
// call says so explicitly, and the guard test uses the strict policy instead.
// The same shape WP-3.2a's plugin tests use.
func webhookServer(t *testing.T, h http.Handler) (*httptest.Server, *atomic.Int32, outbound.Policy) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, &hits, outbound.Policy{AllowPrivateNetworks: true, RootCAs: pool}
}

// registerWebhook stores the URL in the vault and registers the destination
// **through the HTTP surface**, which is what an administrator does.
func (e *env) registerWebhook(t *testing.T, id, url string) (int, map[string]any) {
	t.Helper()
	if status, body := e.putSecret(id+"-url", url); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT secret = %d: %s", status, body)
	}
	host := strings.TrimPrefix(url, "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	status, _, out := e.call("PUT", "/api/v1/webhooks/destinations/"+id, e.token, idgen.New(),
		map[string]any{"host": host, "secret_name": id + "-url", "description": "ops channel"})
	return status, out
}

// drainWebhooks runs the queued webhook jobs with a runner wired to policy,
// returning how many ran. The feed sweep only *enqueues* — the send is a job,
// for the reason call_plugin is (docs/notes/WP-3.3c-decisions.md §4).
// skew advances the runner's clock, which is how a test drives a job a previous
// pass failed: Fail reschedules with an exponential backoff measured from the
// caller's own clock, so re-running at the same instant finds nothing due.
func drainWebhooks(t *testing.T, db *storage.DB, tenant tenancy.ID, keys secrets.KeySource, policy outbound.Policy, skew time.Duration) int {
	t.Helper()
	host := pluginHost(db, mustCRUDObjects(t), keys)
	runner := automations.NewRunner(db,
		crudObjectsAdapter{db: db, cruds: host.Objects},
		pluginEnqueueAdapter{db: db}, keys, policy)
	reg := jobs.NewRegistry()
	reg.Register(automations.WebhookJobKind, runner.WebhookHandler())
	ran, err := jobs.NewRunner(db, reg, "test").RunOnce(context.Background(), tenant, time.Now().UTC().Add(skew))
	if err != nil {
		t.Fatalf("job RunOnce: %v", err)
	}
	return ran
}

// fireWebhookAutomation seeds a destination, an automation and a triggering
// write, then delivers the feed pass. It returns nothing: what each test
// asserts differs, and the queued job is drained by the caller with whatever
// policy it is testing.
func fireWebhookAutomation(t *testing.T, e *env, db *storage.DB, url string) {
	t.Helper()
	if status, out := e.registerWebhook(t, "ops-slack", url); status != http.StatusOK {
		t.Fatalf("PUT destination = %d: %v", status, out)
	}
	status, out := e.postAutomation(t, `
id: notify-ops
name: Notify ops
trigger:
  object: Contact
condition: 'record.kind == "customer"'
actions:
  - type: webhook
    destination: ops-slack
    body:
      severity: high
`)
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/automations = %d: %v", status, out)
	}
	if code, _, created := e.call("POST", "/api/v1/contact", e.token, idgen.New(),
		map[string]any{"name": "Ada Lovelace", "email": idgen.New() + "@example.com", "kind": "customer"}); code >= 300 {
		t.Fatalf("create contact = %d: %v", code, created)
	}
	if fired := runAutomations(t, db, e.tenant); fired != 1 {
		t.Fatalf("the automation fired %d times, want 1", fired)
	}
}

// AC clause 2: audited exactly once per call, attributed to the automation, and
// carrying neither the credential nor the query string.
func TestWebhookIsAuditedExactlyOncePerCall(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			keys := testKeySource(t)
			var gotBody string
			srv, hits, policy := webhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, 2048)
				n, _ := r.Body.Read(b)
				gotBody = string(b[:n])
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			e := seed(t, db)
			url := srv.URL + "/services/" + webhookPathSecret + "?token=SUPERSECRET"
			fireWebhookAutomation(t, e, db, url)

			if ran := drainWebhooks(t, db, e.tenant, keys, policy, 0); ran != 1 {
				t.Fatalf("%d webhook jobs ran, want 1", ran)
			}
			// Non-vacuity: the receiver really was called, or every assertion
			// below is about a call that never happened.
			if hits.Load() != 1 {
				t.Fatalf("the receiver was hit %d times, want 1", hits.Load())
			}
			if !strings.Contains(gotBody, `"automation":"notify-ops"`) || !strings.Contains(gotBody, `"severity":"high"`) {
				t.Fatalf("the envelope is not what the definition asked for: %s", gotBody)
			}
			// No record fields left the tenant: the receiver gets an id (§3).
			if strings.Contains(gotBody, "Ada Lovelace") {
				t.Fatalf("the webhook body carried record fields: %s", gotBody)
			}

			rows := outboundAuditRows(t, db, e.tenant)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want exactly 1 per call", len(rows))
			}
			row := rows[0]
			if row.actor != "automation:notify-ops" {
				t.Fatalf("audit actor = %q, want the automation's principal", row.actor)
			}
			if row.object != "automation" || !strings.Contains(row.changes, `"automation":"notify-ops"`) {
				t.Fatalf("the audit row does not name the automation: object=%q %s", row.object, row.changes)
			}
			for _, want := range []string{`"method":"POST"`, `"host":"` + hostOf(srv.URL) + `"`, `"path":"/services/`} {
				if !strings.Contains(row.changes, want) {
					t.Fatalf("the audit row does not record %s: %s", want, row.changes)
				}
			}
			// INV-K1: the path is the credential and the query string carries
			// API keys. Neither is written down.
			for _, leak := range []string{webhookPathSecret, "SUPERSECRET",
				base64.StdEncoding.EncodeToString([]byte(webhookPathSecret))} {
				if strings.Contains(row.changes, leak) {
					t.Fatalf("the audit row leaked %q: %s", leak, row.changes)
				}
			}

			// A second firing is a second row, not a second-and-a-half: one row
			// per call is the claim.
			if code, _, _ := e.call("POST", "/api/v1/contact", e.token, idgen.New(),
				map[string]any{"name": "Grace Hopper", "email": idgen.New() + "@example.com", "kind": "customer"}); code >= 300 {
				t.Fatal("create second contact failed")
			}
			runAutomations(t, db, e.tenant)
			drainWebhooks(t, db, e.tenant, keys, policy, 0)
			if got := len(outboundAuditRows(t, db, e.tenant)); got != 2 {
				t.Fatalf("audit rows after two calls = %d, want 2", got)
			}
			if hits.Load() != 2 {
				t.Fatalf("the receiver was hit %d times after two firings", hits.Load())
			}
		})
	}
}

// AC clause 3: the SSRF guard covers the new path. It is the same guard, in the
// same dialer — this proves the automation route actually reaches it.
func TestWebhookIsRefusedByTheDialGuard(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			keys := testKeySource(t)
			srv, hits, permissive := webhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			e := seed(t, db)
			fireWebhookAutomation(t, e, db, srv.URL+"/services/"+webhookPathSecret)

			// The default deployment posture. The test server is on loopback,
			// which is exactly what the guard exists to refuse — an approved
			// DNS name resolving into the private network is the same case.
			strict := permissive
			strict.AllowPrivateNetworks = false
			drainWebhooks(t, db, e.tenant, keys, strict, 0)
			if hits.Load() != 0 {
				t.Fatalf("the blocked dial still reached the receiver %d times", hits.Load())
			}
			// A call refused at the socket still wrote its row: the audit is
			// deliberately ahead of the dial, so "no call happens without a
			// row" holds in the direction that matters (WP-3.2a).
			if got := len(outboundAuditRows(t, db, e.tenant)); got != 1 {
				t.Fatalf("audit rows after a blocked dial = %d, want 1", got)
			}
			// The failure is visible to an operator where they look for it,
			// rather than only in a job row.
			if !strings.Contains(webhookRunDetail(t, db, e.tenant), "blocked") {
				t.Fatalf("the refusal is not in the automation's run log: %s", webhookRunDetail(t, db, e.tenant))
			}

			// Mirrored non-vacuity: the identical call succeeds once the
			// deployment opts into private destinations. Without this half the
			// refusal above proves only that the receiver was unreachable.
			drainWebhooks(t, db, e.tenant, keys, permissive, time.Hour)
			if hits.Load() != 1 {
				t.Fatalf("the receiver was hit %d times under the permissive policy", hits.Load())
			}
		})
	}
}

// A 302 to a host nobody registered is the one-hop allowlist bypass. The
// redirect is not followed; the status comes back and the send is recorded as
// failed, because a 3xx is not a delivery.
func TestWebhookDoesNotFollowARedirect(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			keys := testKeySource(t)
			elsewhere, elsewhereHits, _ := webhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			srv, hits, policy := webhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+"/stolen", http.StatusFound)
			}))
			e := seed(t, db)
			fireWebhookAutomation(t, e, db, srv.URL+"/services/"+webhookPathSecret)

			drainWebhooks(t, db, e.tenant, keys, policy, 0)
			if hits.Load() != 1 {
				t.Fatalf("the registered destination was hit %d times, want 1", hits.Load())
			}
			if elsewhereHits.Load() != 0 {
				t.Fatal("the redirect was followed to a host nobody registered")
			}
			// One call, one audit row: the hop that did not happen wrote none.
			if got := len(outboundAuditRows(t, db, e.tenant)); got != 1 {
				t.Fatalf("audit rows = %d, want 1", got)
			}
		})
	}
}

// AC clause 1 at the edge. Registering a destination is `Webhook:manage` —
// deciding where this deployment may call out — and it is not the same power as
// writing an automation.
func TestDestinationRoutesRequireManage(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			_ = testKeySource(t)
			e := seed(t, db)

			// A user who can write automations and even send webhooks, but may
			// not decide where.
			weak := e.issueUser(t, map[string][]string{
				"Automation":           {"manage"},
				"Contact":              {"create", "read", "update"},
				"secret":               {"manage"},
				outbound.ObjectWebhook: {outbound.ActionSend},
			})
			body := map[string]any{"host": "hooks.example.com", "secret_name": "x-url"}
			for _, probe := range []struct{ method, path string }{
				{"GET", "/api/v1/webhooks/destinations"},
				{"PUT", "/api/v1/webhooks/destinations/ops"},
				{"DELETE", "/api/v1/webhooks/destinations/ops"},
			} {
				code, _, _ := e.call(probe.method, probe.path, weak, idgen.New(), body)
				if code != http.StatusForbidden {
					t.Fatalf("%s %s as a Webhook:send holder = %d, want 403", probe.method, probe.path, code)
				}
			}
			// Mirrored: the administrator's token, which holds `manage`, is
			// allowed — or the 403s above would only mean the routes are broken.
			if code, _, _ := e.call("GET", "/api/v1/webhooks/destinations", e.token, "", nil); code != http.StatusOK {
				t.Fatalf("GET destinations as an administrator = %d, want 200", code)
			}
		})
	}
}

// INV-K1's structural half for this surface, in the shape WP-3.0's
// TestNoRouteReturnsASecret takes: the destination's URL is a credential, so no
// route returns it — asserted against the live server rather than against
// review. The automation document is swept too, because that is the other place
// a URL would have ended up if the design had put it there.
func TestNoRouteReturnsAWebhookURL(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			keys := testKeySource(t)
			srv, _, policy := webhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			e := seed(t, db)
			fireWebhookAutomation(t, e, db, srv.URL+"/services/"+webhookPathSecret)
			drainWebhooks(t, db, e.tenant, keys, policy, 0)

			encoded := base64.StdEncoding.EncodeToString([]byte(webhookPathSecret))
			for _, path := range []string{
				"/api/v1/webhooks/destinations",
				"/api/v1/webhooks/destinations/ops-slack",
				"/api/v1/secrets",
				"/api/v1/automations",
				"/api/v1/automations/notify-ops",
				"/api/v1/automations/notify-ops/runs",
			} {
				_, body, _ := e.call("GET", path, e.token, "", nil)
				for _, leak := range []string{webhookPathSecret, encoded} {
					if strings.Contains(string(body), leak) {
						t.Fatalf("GET %s returned the webhook credential: %s", path, body)
					}
				}
			}
			// Non-vacuity: the destination is really there to be leaked, or
			// this sweep is reading 404s.
			_, body, _ := e.call("GET", "/api/v1/webhooks/destinations/ops-slack", e.token, "", nil)
			if !strings.Contains(string(body), "ops-slack") {
				t.Fatalf("the destination route returned nothing to sweep: %s", body)
			}

			// And the whole audit trail, raw and base64 — WP-3.0's sweep
			// extended down the new path.
			for _, row := range allAuditRows(t, db, e.tenant) {
				if strings.Contains(row.changes, webhookPathSecret) || strings.Contains(row.changes, encoded) {
					t.Fatalf("an audit row leaked the webhook credential: %s", row.changes)
				}
			}
		})
	}
}

// --- helpers ---

type outboundAudit struct{ action, object, actor, changes string }

func outboundAuditRows(t *testing.T, db *storage.DB, tenant tenancy.ID) []outboundAudit {
	t.Helper()
	var out []outboundAudit
	for _, row := range allAuditRows(t, db, tenant) {
		if row.action == "http.request" {
			out = append(out, row)
		}
	}
	return out
}

func allAuditRows(t *testing.T, db *storage.DB, tenant tenancy.ID) []outboundAudit {
	t.Helper()
	var out []outboundAudit
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var list []outboundAudit
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT action, object, actor_id, changes FROM audit_log WHERE tenant_id = ? ORDER BY at`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r outboundAudit
			if err := rows.Scan(&r.action, &r.object, &r.actor, &r.changes); err != nil {
				return err
			}
			list = append(list, r)
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

// webhookRunDetail is the newest run's detail for the webhook automation, which
// is where an operator looks for "did my webhook fire".
func webhookRunDetail(t *testing.T, db *storage.DB, tenant tenancy.ID) string {
	t.Helper()
	runs, err := automations.Runs(context.Background(), db, tenant, "notify-ops", 0)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	var out []string
	for _, r := range runs {
		out = append(out, r.Outcome+":"+r.Detail)
	}
	return strings.Join(out, " | ")
}

func hostOf(rawURL string) string {
	host := strings.TrimPrefix(rawURL, "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return host
}

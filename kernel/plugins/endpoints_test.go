// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.2a, the inbound half: routes a plugin declares under `/ext/<id>/`.
// Invariants: INV-X1 (an endpoint runs with the plugin's own approved
// authority, nothing more), INV-T4 (what it writes is attributed to the
// plugin, not to whoever called the route).

// declaredEndpoints is the manifest block for the web module's three routes.
const declaredEndpoints = `
endpoints:
  - {path: /report, fn: report, methods: [GET, POST]}
  - {path: /naughty, fn: naughty}
  - {path: /write, fn: write, methods: [POST]}
`

// endpointHost is a Host that can serve endpoints and address Widget.
func endpointHost(t *testing.T, db *storage.DB) Host {
	t.Helper()
	return Host{
		DB:      db,
		Objects: map[string]*metadata.CRUD{"Widget": widgetCRUD(t, db)},
		Limits:  DefaultLimits,
	}
}

// installWeb installs the web module with its endpoints. The http allowlist is
// present but points nowhere these tests call: the module *imports*
// lasterp_http_request, and an import the manifest does not cover is a module
// that cannot instantiate — INV-X1's mechanism, met here as a fact of life.
func installWeb(t *testing.T, db *storage.DB, tenant tenancy.ID) *Installed {
	t.Helper()
	manifest := webManifest(httpTo("api.example.com")) + declaredEndpoints
	return install(t, db, tenant, manifest, corpusModule(t, "web"), helloApprover(t, db, tenant))
}

func TestEndpointServesADeclaredRoute(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := installWeb(t, db, tenant)

			res, err := ServeEndpoint(context.Background(), endpointHost(t, db), tenant, p, EndpointRequest{
				Method: "POST", Path: "/report", Query: "since=2026-01-01", Body: `{"x":1}`, Caller: "user-7",
			})
			if err != nil {
				t.Fatalf("ServeEndpoint: %v", err)
			}
			if res.Status != http.StatusOK || res.ContentType != "application/json" {
				t.Fatalf("status %d type %q", res.Status, res.ContentType)
			}

			// The plugin echoes what it was told, which is how we check what it
			// was told: the caller's identity yes, the caller's credentials
			// never — EndpointRequest has no header field at all, and this is
			// that structure observed from the plugin's side.
			var seen map[string]any
			if err := json.Unmarshal(res.Body, &seen); err != nil {
				t.Fatalf("plugin body is not JSON (%s): %v", res.Body, err)
			}
			for field, want := range map[string]string{
				"method": "POST", "path": "/report", "query": "since=2026-01-01",
				"body": `{"x":1}`, "caller": "user-7",
			} {
				if got, _ := seen[field].(string); got != want {
					t.Fatalf("plugin saw %s = %q, want %q", field, got, want)
				}
			}
			if len(seen) != 5 {
				t.Fatalf("the plugin was handed %d fields: %v — the request payload grew", len(seen), seen)
			}
		})
	}
}

// TestEndpointResponseIsClamped: whatever the module returns, the server signs
// its own origin to the result. HTML and a redirect status are the two that
// matter — one is scripting rights in the caller's session, the other is an
// open redirect from a trusted host.
func TestEndpointResponseIsClamped(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := installWeb(t, db, tenant)

			res, err := ServeEndpoint(context.Background(), endpointHost(t, db), tenant, p, EndpointRequest{
				Method: "GET", Path: "/naughty", Caller: "user-7",
			})
			if err != nil {
				t.Fatalf("ServeEndpoint: %v", err)
			}
			if res.Status != http.StatusOK {
				t.Fatalf("status %d: the plugin's 302 was passed through", res.Status)
			}
			if res.ContentType != "application/json" {
				t.Fatalf("content type %q: the plugin chose its own", res.ContentType)
			}
			// The body itself is the plugin's to write — it is data, not markup,
			// once the content type says so.
			if !strings.Contains(string(res.Body), "<script>") {
				t.Fatalf("body = %q, expected the plugin's own bytes", res.Body)
			}
		})
	}
}

func TestUndeclaredRouteOrMethodIsNotServed(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := installWeb(t, db, tenant)
			h := endpointHost(t, db)

			for what, req := range map[string]EndpointRequest{
				"undeclared path":     {Method: "GET", Path: "/admin"},
				"undeclared method":   {Method: "GET", Path: "/write"},
				"path traversal":      {Method: "GET", Path: "/../report"},
				"trailing difference": {Method: "GET", Path: "/report/"},
			} {
				if _, err := ServeEndpoint(context.Background(), h, tenant, p, req); !errors.Is(err, ErrNoSuchEndpoint) {
					t.Fatalf("%s: err = %v, want ErrNoSuchEndpoint", what, err)
				}
			}
		})
	}
}

// TestEndpointWritesAsThePlugin is INV-T4 through the inbound surface: a record
// created from an endpoint names the plugin, not the person who called the
// route. A plugin that wrote as its caller would make the audit trail say a
// user created a row they never saw.
func TestEndpointWritesAsThePlugin(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := installWeb(t, db, tenant)

			res, err := ServeEndpoint(context.Background(), endpointHost(t, db), tenant, p, EndpointRequest{
				Method: "POST", Path: "/write", Caller: "user-7",
			})
			if err != nil {
				t.Fatalf("ServeEndpoint: %v", err)
			}
			if res.Status != http.StatusCreated || !strings.Contains(string(res.Body), `"ok":true`) {
				t.Fatalf("the endpoint's write failed: %d %s", res.Status, res.Body)
			}

			var found bool
			for _, row := range auditRows(t, db, tenant, "create") {
				if row.object != "Widget" {
					continue
				}
				found = true
				if row.actor != p.Principal() {
					t.Fatalf("Widget was created by %q, want %q", row.actor, p.Principal())
				}
				if row.actor == "user-7" {
					t.Fatal("the write was attributed to the caller")
				}
			}
			if !found {
				t.Fatal("no Widget create was audited — the assertion above ran on nothing")
			}
		})
	}
}

func TestManifestRefusesUnroutableEndpoints(t *testing.T) {
	for what, endpoints := range map[string]string{
		"relative path":      "\nendpoints:\n  - {path: report, fn: report}\n",
		"traversal":          "\nendpoints:\n  - {path: /../etc, fn: report}\n",
		"unrouted method":    "\nendpoints:\n  - {path: /report, fn: report, methods: [PUT]}\n",
		"undeclared fn":      "\nendpoints:\n  - {path: /report, fn: nowhere}\n",
		"duplicated path":    "\nendpoints:\n  - {path: /report, fn: report}\n  - {path: /report, fn: naughty}\n",
		"wildcard http host": "capabilities:\n  http:\n    - {host: \"*.acme.com\"}\n",
	} {
		manifest := "id: com.acme.web\nversion: 1.0.0\nfunctions: [report, naughty]\n" + endpoints
		if _, err := ParseManifest([]byte(manifest)); err == nil {
			t.Errorf("%s: manifest was accepted", what)
		}
	}

	// Non-vacuity: the shape the docs promise parses.
	ok := "id: com.acme.web\nversion: 1.0.0\nfunctions: [report]\ncapabilities:\n  http:\n    - {host: api.acme.com, methods: [GET, POST]}\nendpoints:\n  - {path: /report, fn: report, methods: [GET]}\n"
	if _, err := ParseManifest([]byte(ok)); err != nil {
		t.Fatalf("the documented manifest was refused: %v", err)
	}
}

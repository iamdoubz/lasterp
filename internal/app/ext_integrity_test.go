//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-3.2a: `/ext/<plugin>/` over the wired product handler.
//
// Invariants: **INV-T2** (no unauthenticated path to plugin code), INV-T3 (the
// caller's grants never become the plugin's, in either direction), INV-T4 (an
// endpoint's writes name the plugin), **INV-X1** (an endpoint reaches only what
// its manifest declared and an administrator approved).
//
// The kernel suite proves the same rules against ServeEndpoint; this one proves
// they survive the gateway — the authn guard, the capability gate, idempotency
// on writes, and the response the browser actually receives.

// raw is env.call with the response left open, because the two things this WP
// adds to the wire — the content type a plugin may choose and the nosniff
// header it may not — are headers, and env.call returns only the body.
func (e *env) raw(t *testing.T, method, path, token, idem string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

var (
	webCorpusOnce sync.Once
	webCorpusWasm []byte
	webCorpusErr  error
)

func webModule(t *testing.T) []byte {
	t.Helper()
	webCorpusOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lasterp-app-web")
		if err != nil {
			webCorpusErr = err
			return
		}
		out := filepath.Join(dir, "web.wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./web")
		cmd.Dir = filepath.Join("..", "..", "kernel", "plugins", "testdata")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if combined, err := cmd.CombinedOutput(); err != nil {
			webCorpusErr = &corpusBuildError{out: string(combined), err: err}
			return
		}
		webCorpusWasm, webCorpusErr = os.ReadFile(out)
	})
	if webCorpusErr != nil {
		t.Fatalf("build web plugin: %v", webCorpusErr)
	}
	return webCorpusWasm
}

// extManifest declares the web module against the product's Contact object.
// `access` is the whole point of the parameter: read-only is a plugin that
// cannot write no matter who calls its endpoint.
func extManifest(access string) string {
	return `
id: com.acme.web
version: 1.0.0
functions: [fetch, exfiltrate, report, naughty, write]
capabilities:
  objects:
    - {type: Contact, access: ` + access + `}
  secrets: [acme_api_key]
  http:
    - {host: api.example.com, methods: [GET, POST]}
endpoints:
  - {path: /report, fn: report, methods: [GET, POST]}
  - {path: /naughty, fn: naughty}
  - {path: /write, fn: write, methods: [POST]}
`
}

func installWebPlugin(t *testing.T, e *env, access string) {
	t.Helper()
	status, body, _ := e.post("/api/v1/plugins", map[string]any{
		"manifest": extManifest(access),
		"module":   base64.StdEncoding.EncodeToString(webModule(t)),
	})
	if status != http.StatusCreated {
		t.Fatalf("install web plugin = %d: %s", status, body)
	}
	// The approval screen has to show what it is approving: neither the
	// outbound allowlist nor the served routes is an authz permission the
	// install could check against the approver's grants, so this listing *is*
	// the review (WP-3.2-decisions.md §1).
	for _, want := range []string{`"/ext/com.acme.web/report"`, `"api.example.com"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the install response does not show %s: %s", want, body)
		}
	}
}

func TestExtRouteServesADeclaredPluginRoute(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installWebPlugin(t, e, "write")

			resp := e.raw(t, "GET", "/ext/com.acme.web/report?since=2026-01-01", e.token, "", nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET the plugin's route = %d", resp.StatusCode)
			}
			// The two headers a browser acts on. nosniff because the body is
			// untrusted output on our own origin.
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q", got)
			}
			var seen map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&seen); err != nil {
				t.Fatalf("plugin body: %v", err)
			}
			if seen["query"] != "since=2026-01-01" || seen["path"] != "/report" {
				t.Fatalf("the plugin was told %v", seen)
			}
			// It learns who called; it never learns how they authenticated.
			if seen["caller"] == "" || seen["caller"] == nil {
				t.Fatal("the plugin was not told who called it")
			}
			for _, forbidden := range []string{"headers", "authorization", "cookie", "token"} {
				if _, ok := seen[forbidden]; ok {
					t.Fatalf("the plugin was handed %q", forbidden)
				}
			}
		})
	}
}

// TestExtRouteIsNotAWayInWithoutASession is INV-T2 at the new surface: /ext is
// outside /api/v1, and a route that skipped the gateway's guard would be the
// hole the plugin host spent two WPs not opening.
func TestExtRouteIsNotAWayInWithoutASession(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installWebPlugin(t, e, "write")

			resp := e.raw(t, "GET", "/ext/com.acme.web/report", "", "", nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated GET = %d, want 401", resp.StatusCode)
			}

			// A session is not enough: running plugin code needs `plugin:invoke`,
			// the same permission as calling a function directly.
			token := e.issueUser(t, map[string][]string{"Contact": {"create", "read"}})
			resp2 := e.raw(t, "GET", "/ext/com.acme.web/report", token, "", nil)
			defer func() { _ = resp2.Body.Close() }()
			if resp2.StatusCode != http.StatusForbidden {
				t.Fatalf("GET without plugin:invoke = %d, want 403", resp2.StatusCode)
			}

			// Non-vacuity: with invoke, the same call works — so the two
			// refusals above are the gate, not a broken route.
			invoker := e.issueUser(t, map[string][]string{"plugin": {"invoke"}})
			resp3 := e.raw(t, "GET", "/ext/com.acme.web/report", invoker, "", nil)
			defer func() { _ = resp3.Body.Close() }()
			if resp3.StatusCode != http.StatusOK {
				t.Fatalf("GET with plugin:invoke = %d, want 200", resp3.StatusCode)
			}
		})
	}
}

// TestExtEndpointActsAsThePluginNotItsCaller is INV-T3 in both directions: a
// caller with no Contact grant still gets the plugin's declared write, and a
// caller with every Contact grant cannot make a read-only plugin write.
func TestExtEndpointActsAsThePluginNotItsCaller(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installWebPlugin(t, e, "write")

			// A caller who may invoke plugins and nothing else.
			invoker := e.issueUser(t, map[string][]string{"plugin": {"invoke"}})
			body := map[string]any{"object": "Contact", "record": map[string]any{
				"name": "made by com.acme.web", "kind": "customer"}}
			resp := e.raw(t, "POST", "/ext/com.acme.web/write", invoker, idgen.New(), body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("the plugin's own write = %d, want 201", resp.StatusCode)
			}

			// And the row is the plugin's, not the caller's (INV-T4).
			status, listBody, parsed := e.get("/api/v1/contact")
			if status != http.StatusOK {
				t.Fatalf("list contacts = %d: %s", status, listBody)
			}
			if !strings.Contains(string(listBody), "made by com.acme.web") {
				t.Fatalf("the plugin's contact is missing: %v", parsed)
			}
		})
	}
}

func TestExtEndpointCannotBorrowTheCallersGrants(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			// Read-only manifest: the module still *imports* object_create, and
			// an import the approval does not cover is a module that cannot be
			// instantiated at all. INV-X1 is structural, so a caller holding
			// every Contact grant changes nothing.
			installWebPlugin(t, e, "read")

			body := map[string]any{"object": "Contact", "record": map[string]any{
				"name": "smuggled by the caller", "kind": "customer"}}
			// e.token holds the full grant set, Contact create included.
			resp := e.raw(t, "POST", "/ext/com.acme.web/write", e.token, idgen.New(), body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusCreated {
				t.Fatal("a read-only plugin wrote a Contact on its caller's authority")
			}

			_, listBody, _ := e.get("/api/v1/contact")
			if strings.Contains(string(listBody), "smuggled by the caller") {
				t.Fatal("the write landed anyway")
			}
		})
	}
}

func TestExtRouteRefusesUnknownPluginsAndPaths(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installWebPlugin(t, e, "write")

			for _, path := range []string{
				"/ext/com.acme.nothing/report", // no such plugin
				"/ext/com.acme.web/admin",      // not declared
				"/ext/com.acme.web/write",      // declared, but POST only
			} {
				resp := e.raw(t, "GET", path, e.token, "", nil)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("GET %s = %d, want 404", path, resp.StatusCode)
				}
			}
		})
	}
}

// TestPluginListingShowsWhatItExposes: an administrator can answer "what does
// this thing expose, and where does it call out" from the management API,
// without reading the module or the manifest file.
func TestPluginListingShowsWhatItExposes(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e, _ := seedVault(t, db)
			installWebPlugin(t, e, "write")

			status, body, _ := e.get("/api/v1/plugins")
			if status != http.StatusOK {
				t.Fatalf("list plugins = %d: %s", status, body)
			}
			for _, want := range []string{
				`"/ext/com.acme.web/report"`, `"/ext/com.acme.web/write"`, `"api.example.com"`,
			} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("the listing does not show %s: %s", want, body)
				}
			}
		})
	}
}

// TestExtRouteIsEnumerableFromTheActionTable: the route fence works only if the
// wildcard pattern is a declared action like any other. A plugin surface added
// by mutating the mux would be invisible to every route-enumeration test in
// this package.
func TestExtRouteIsEnumerableFromTheActionTable(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			var methods []string
			for _, a := range allActions(t, db) {
				if strings.HasPrefix(a.Path, "/ext/") {
					methods = append(methods, a.Method)
					if a.Public {
						t.Fatalf("%s %s is public: /ext is not an anonymous surface", a.Method, a.Path)
					}
					if a.Object == "" {
						t.Fatalf("%s %s is capability-ungated", a.Method, a.Path)
					}
				}
			}
			if len(methods) != 2 {
				t.Fatalf("/ext actions = %v, want exactly GET and POST", methods)
			}
		})
	}
}

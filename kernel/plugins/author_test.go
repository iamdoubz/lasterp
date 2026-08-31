// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// WP-3.2b, the author's half: scaffolds, bindings, the host-version range and
// the registry client. No invariants of its own — nothing here reaches a
// tenant's data — but the AC "an afternoon-plugin tutorial completes" is only
// true if the scaffold this generates actually builds, which is what
// TestGoScaffoldCompiles asserts rather than assumes.

func TestScaffoldsRenderAndParse(t *testing.T) {
	for _, lang := range ScaffoldLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			written, err := NewPlugin(dir, lang, "com.acme.afternoon")
			if err != nil {
				t.Fatalf("NewPlugin: %v", err)
			}
			if len(written) < 3 {
				t.Fatalf("wrote only %v", written)
			}

			// Every scaffold carries a manifest *this host* accepts. A starter
			// project whose first `lasterp plugin install` fails on its own
			// generated manifest is the worst possible first five minutes.
			raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			manifest, err := ParseManifest(raw)
			if err != nil {
				t.Fatalf("the generated manifest does not parse: %v", err)
			}
			if manifest.ID != "com.acme.afternoon" {
				t.Fatalf("manifest id = %q", manifest.ID)
			}
			if _, ok := manifest.Endpoint("/hello"); !ok {
				t.Fatal("the scaffold does not declare the route its code serves")
			}
			// The id is interpolated everywhere, so nothing in a fresh project
			// still says "com.acme.example".
			for _, file := range written {
				body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				if strings.Contains(string(body), "{{") {
					t.Fatalf("%s still holds an unrendered template action", file)
				}
			}
		})
	}
}

func TestScaffoldRefusesBadInputAndNonEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewPlugin(dir, "cobol", "com.acme.x"); err == nil {
		t.Error("an unknown language was accepted")
	}
	if _, err := NewPlugin(dir, "go", "Com.Acme!"); err == nil {
		t.Error("an invalid plugin id was accepted")
	}
	if _, err := NewPlugin(dir, "go", "com.acme.x"); err != nil {
		t.Fatalf("NewPlugin: %v", err)
	}
	// Scaffolding twice would overwrite an afternoon of somebody's work.
	if _, err := NewPlugin(dir, "go", "com.acme.x"); err == nil {
		t.Error("scaffolding over a non-empty directory was allowed")
	}
}

// TestGoScaffoldCompiles is the one scaffold CI proves end to end (decisions
// §5): the Go template is compiled to wasip1/wasm by the toolchain this repo
// already pins, and the result is a module this host can instantiate.
func TestGoScaffoldCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a wasm module")
	}
	dir := t.TempDir()
	if _, err := NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
		t.Fatalf("NewPlugin: %v", err)
	}
	out := filepath.Join(dir, "plugin.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the Go scaffold does not build:\n%s\n%v", combined, err)
	}
	module, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read module: %v", err)
	}
	if !isWASM(module) {
		t.Fatal("the scaffold's build output is not a WebAssembly module")
	}
}

// --- the host version range ---

func TestHostVersionRange(t *testing.T) {
	// HostVersion is 0.1.0 today, so these are written against that rather than
	// against a version we wish we had.
	for rng, want := range map[string]bool{
		"":             true, // no claim
		">=0.1":        true,
		">=0.1.0 <1.0": true,
		"=0.1.0":       true,
		"0.1.0":        true,
		">=1.0 <2.0":   false, // the docs' own illustrative range: not yet
		">0.1.0":       false,
		"<0.1":         false,
		">=0.1 <0.1.0": false,
	} {
		err := checkHostVersion(rng)
		if want && err != nil {
			t.Errorf("%q was refused: %v", rng, err)
		}
		if !want && err == nil {
			t.Errorf("%q was accepted", rng)
		}
	}
	for _, bad := range []string{">=banana", ">=1.2.3.4", "~>1.0", ">=1.0-rc1"} {
		if err := checkHostVersion(bad); err == nil {
			t.Errorf("%q was accepted as a range", bad)
		}
	}
}

// TestHostVersionMatchesTheChart keeps the one version this product writes down
// from drifting into two.
func TestHostVersionMatchesTheChart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "helm", "lasterp", "Chart.yaml"))
	if err != nil {
		t.Skipf("no chart to compare against: %v", err)
	}
	want := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "appVersion:"); ok {
			want = strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	if want == "" {
		t.Skip("the chart declares no appVersion")
	}
	if want != HostVersion {
		t.Fatalf("plugins.HostVersion is %s and the Helm chart's appVersion is %s — a plugin's `lasterp:` range would be checked against a version this deployment does not call itself", HostVersion, want)
	}
}

// --- bindings ---

func TestGeneratedBindingsCompile(t *testing.T) {
	objects := []MetaObject{
		{Name: "Invoice", Module: "invoicing", Persistence: "crud", Fields: []MetaField{
			{Name: "contact_id", Type: "link", Required: true},
			{Name: "status", Type: "enum", Required: true, Options: []string{"draft", "posted"}},
			{Name: "net_minor", Type: "int"},
			{Name: "total", Type: "money"},
		}},
		{Name: "Contact", Module: "contacts", Persistence: "crud", Fields: []MetaField{
			{Name: "name", Type: "text", Required: true},
		}},
	}
	src, err := GenerateGoBindings("plugin", objects)
	if err != nil {
		t.Fatalf("GenerateGoBindings: %v", err)
	}
	// Compared with whitespace collapsed: gofmt aligns struct fields, and a test
	// that depends on how many spaces it chose breaks when an unrelated field is
	// renamed.
	flat := strings.Join(strings.Fields(string(src)), " ")
	for _, want := range []string{
		"type Invoice struct", "type Contact struct",
		"ObjectInvoice = \"Invoice\"",
		"ContactID string", // link → string, and the initialism is fixed up
		"NetMinor int64",
		"Total Money", // money is never a float
		"// one of: draft, posted",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated bindings do not contain %q:\n%s", want, src)
		}
	}

	if testing.Short() {
		t.Skip("compiles the generated file")
	}
	// The claim in decisions §6 is that Go's output type-checks. This is that
	// claim, not a promise of it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/bindings\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects.go"), src, 0o600); err != nil {
		t.Fatalf("write objects.go: %v", err)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated bindings do not compile:\n%s\n%v", out, err)
	}
}

func TestParseMetaObjectsReadsTheEndpointEnvelope(t *testing.T) {
	body := []byte(`{"data":[{"name":"Contact","module":"contacts","persistence":"crud",
		"fields":[{"name":"kind","type":"enum","required":true,"options":["customer","vendor"]}]}]}`)
	objects, err := ParseMetaObjects(body)
	if err != nil {
		t.Fatalf("ParseMetaObjects: %v", err)
	}
	if len(objects) != 1 || objects[0].Name != "Contact" || len(objects[0].Fields) != 1 {
		t.Fatalf("parsed %+v", objects)
	}
	if !objects[0].Fields[0].Required || objects[0].Fields[0].Options[0] != "customer" {
		t.Fatalf("field detail lost: %+v", objects[0].Fields[0])
	}
}

// --- the registry ---

func TestRegistryResolvesAndFetches(t *testing.T) {
	key := testSigningKey(t)
	bundle, err := Pack([]byte(helloWith("echo")), corpusModule(t, "hello"), key.ID, key.Key)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	opened, err := OpenBundle(bundle)
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}

	index := Index{Plugins: []IndexEntry{
		{ID: "com.acme.hello", Version: "0.9.0", Bundle: "hello-0.9.0.tar.gz"},
		{ID: "com.acme.hello", Version: "1.2.0", Bundle: "hello-1.2.0.tar.gz", Digest: opened.Digest},
		{ID: "com.acme.hello", Version: "1.10.0", Bundle: "hello-1.10.0.tar.gz", Digest: opened.Digest},
		{ID: "com.acme.other", Version: "1.0.0", Bundle: "other.tar.gz"},
	}}
	body, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json":
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(bundle)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	fetched, err := FetchIndex(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}

	// A bare id means the newest — and 1.10.0 is newer than 1.2.0, which string
	// comparison would get backwards.
	newest, err := fetched.Resolve("com.acme.hello")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if newest.Version != "1.10.0" {
		t.Fatalf("newest resolved to %s", newest.Version)
	}
	exact, err := fetched.Resolve("com.acme.hello@0.9.0")
	if err != nil || exact.Version != "0.9.0" {
		t.Fatalf("exact resolve = %+v, %v", exact, err)
	}
	if _, err := fetched.Resolve("com.acme.nothing"); err == nil {
		t.Error("an unknown id resolved")
	}
	if _, err := fetched.Resolve("com.acme.hello@9.9.9"); err == nil {
		t.Error("an unpublished version resolved")
	}

	got, err := FetchBundle(context.Background(), srv.Client(), srv.URL, newest)
	if err != nil {
		t.Fatalf("FetchBundle: %v", err)
	}
	if len(got) != len(bundle) {
		t.Fatalf("fetched %d bytes, packed %d", len(got), len(bundle))
	}

	// A mirror serving different bytes than the index advertises is caught
	// without anyone needing the publisher's key.
	lying := newest
	lying.Digest = strings.Repeat("0", 64)
	if _, err := FetchBundle(context.Background(), srv.Client(), srv.URL, lying); !errors.Is(err, ErrBundle) {
		t.Fatalf("a digest mismatch was accepted: %v", err)
	}
}

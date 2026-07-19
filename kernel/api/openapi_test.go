// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

var update = flag.Bool("update", false, "regenerate the committed core OpenAPI spec (api/openapi.json)")

// sampleEffective parses+merges the Contact object without touching a DB —
// OpenAPI generation needs only the effective schema.
func sampleEffective(t *testing.T) *metadata.EffectiveSchema {
	t.Helper()
	core, err := metadata.ParseObject([]byte(contactYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	eff, err := metadata.Merge(core)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return eff
}

// TestOpenAPIValidates asserts the generated document has the OpenAPI 3.1
// required shape (the AC's "OpenAPI spec validates"). Full Spectral linting
// is WP-3.7's CI gate (docs/15); this is the structural contract.
func TestOpenAPIValidates(t *testing.T) {
	spec := OpenAPI([]*metadata.EffectiveSchema{sampleEffective(t)}, nil)

	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", spec["openapi"])
	}
	info, ok := spec["info"].(obj)
	if !ok || info["title"] == "" || info["version"] == "" {
		t.Fatalf("info missing title/version: %v", spec["info"])
	}
	paths, ok := spec["paths"].(obj)
	if !ok || len(paths) == 0 {
		t.Fatal("paths missing or empty")
	}

	// The Contact object contributes a collection and an item path.
	for _, p := range []string{"/api/v1/contact", "/api/v1/contact/{id}"} {
		if _, ok := paths[p]; !ok {
			t.Fatalf("missing path %q", p)
		}
	}

	// Every operation must declare responses (OpenAPI requires it).
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	for name, pi := range paths {
		item, ok := pi.(obj)
		if !ok {
			t.Fatalf("path %q is not an object", name)
		}
		for op, node := range item {
			if !methods[op] {
				continue
			}
			operation, ok := node.(obj)
			if !ok {
				t.Fatalf("%s %s is not an operation object", op, name)
			}
			if _, ok := operation["responses"].(obj); !ok {
				t.Fatalf("%s %s has no responses", op, name)
			}
		}
	}

	// Components the paths $ref must resolve.
	components, _ := spec["components"].(obj)
	schemas, _ := components["schemas"].(obj)
	if _, ok := schemas["Problem"]; !ok {
		t.Fatal("components.schemas.Problem missing")
	}
	if _, ok := schemas["Contact"]; !ok {
		t.Fatal("components.schemas.Contact missing")
	}

	// It must round-trip through encoding/json (the wire format).
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("spec does not marshal: %v", err)
	}

	// Required-field propagation: full_name is required in the source object.
	contact, _ := schemas["Contact"].(obj)
	req, _ := contact["required"].([]any)
	if len(req) != 1 || req[0] != "full_name" {
		t.Fatalf("Contact.required = %v, want [full_name]", contact["required"])
	}
}

// TestOpenAPIEndpointServesSpec checks the live endpoint returns the spec.
func TestOpenAPIEndpointServesSpec(t *testing.T) {
	g := NewGateway(Config{Objects: []*metadata.EffectiveSchema{sampleEffective(t)}})
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var spec map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("served spec openapi = %v", spec["openapi"])
	}
}

// TestCoreSpecGolden keeps the committed core spec (api/openapi.json) in sync
// with the generator. The kernel ships no domain objects yet, so the core
// spec is the object-less envelope; module WPs regenerate it with -update as
// they add core objects. Run: go test ./kernel/api -run Golden -update
func TestCoreSpecGolden(t *testing.T) {
	path := filepath.Join("..", "..", "api", "openapi.json")
	got, err := json.MarshalIndent(OpenAPI(nil, nil), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to generate): %v", err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(got, "\n")) {
		t.Fatalf("api/openapi.json is stale; regenerate with: go test ./kernel/api -run Golden -update")
	}
}

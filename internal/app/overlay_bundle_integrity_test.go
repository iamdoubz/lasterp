//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/plugins"
)

// WP-3.2c AC4: a plugin bundle carrying an overlay installs and uninstalls
// cleanly.
//
// The manifest's `overlays:` was refused by name from WP-3.1a until this WP —
// "declared but unimplemented", because there were no per-tenant schemas to
// apply one to. These are the tests that let the refusal be removed: the
// declaration is honoured in full, or the install does not happen (INV-T3), and
// what it changed goes with it on uninstall.

// overlayManifest is the scaffold's manifest with an `overlays:` declaration
// added, since `lasterp plugin new` does not scaffold one.
func overlayManifest(t *testing.T, dir string) string {
	t.Helper()
	return readFile(t, filepath.Join(dir, "manifest.yaml")) + "overlays: [Contact]\n"
}

const bundleOverlay = `object: Contact
add_fields:
  - {name: acme_segment, type: enum, options: [smb, mid, enterprise]}
`

func packOverlayBundle(t *testing.T, key plugins.SigningKey, manifest string, module []byte, overlays map[string][]byte) string {
	t.Helper()
	bundle, err := plugins.Pack([]byte(manifest), module, overlays, key.ID, key.Key)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bundle)
}

func TestPluginBundleOverlayInstallsAndUninstalls(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}
			manifest := overlayManifest(t, dir)
			module := buildWasm(t, dir)
			overlays := map[string][]byte{"Contact": []byte(bundleOverlay)}

			// Before: the field does not exist. Without this the assertion
			// below would pass against a schema that always had it.
			if _, ok := metaFieldsFor(t, e, "Contact")["acme_segment"]; ok {
				t.Fatal("acme_segment exists before the plugin was installed")
			}

			status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": packOverlayBundle(t, key, manifest, module, overlays),
			})
			if status != http.StatusCreated {
				t.Fatalf("install bundle with an overlay = %d, want 201; body=%s", status, body)
			}

			// The schema change is live on the request path, not merely stored.
			if opts := metaFieldsFor(t, e, "Contact")["acme_segment"]; strings.Join(opts, ",") != "smb,mid,enterprise" {
				t.Fatalf("acme_segment options = %v, want the plugin's three", opts)
			}
			st, cbody, rec := e.post("/api/v1/contact", map[string]any{
				"name": "Acme Co", "kind": "customer", "acme_segment": "enterprise",
			})
			if st != http.StatusCreated {
				t.Fatalf("write to the plugin's field = %d, want 201; body=%s", st, cbody)
			}
			if rec["acme_segment"] != "enterprise" {
				t.Fatalf("stored acme_segment = %v, want enterprise", rec["acme_segment"])
			}

			// The overlay is listed with the plugin as its source, so an
			// administrator asking "what changed my schema" is answered.
			var found bool
			for _, o := range listOverlays(t, e) {
				if o["object"] == "Contact" && o["source"] == "com.acme.afternoon" && o["layer"] == "plugin" {
					found = true
				}
			}
			if !found {
				t.Errorf("the plugin's overlay is not listed under its own source: %v", listOverlays(t, e))
			}

			// Uninstall takes the schema change with it, like the role and the
			// kv: an overlay outliving its plugin is a field with no owner.
			if status, body, _ := e.call("DELETE", "/api/v1/plugins/com.acme.afternoon", e.token, "uninstall-1", nil); status != http.StatusOK {
				t.Fatalf("uninstall = %d, want 200; body=%s", status, body)
			}
			if _, ok := metaFieldsFor(t, e, "Contact")["acme_segment"]; ok {
				t.Error("the plugin's field survived its uninstall")
			}
			if got := listOverlays(t, e); len(got) != 0 {
				t.Errorf("uninstall left %d overlays behind: %v", len(got), got)
			}
			// And the write that worked a moment ago is refused, because for
			// this tenant that field no longer exists.
			if st, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "After", "kind": "customer", "acme_segment": "smb",
			}); st == http.StatusCreated {
				t.Errorf("wrote the uninstalled plugin's field; body=%s", body)
			}
		})
	}
}

// INV-T3 through the supply chain: a bundle whose overlay would widen what core
// declared is refused, and nothing about it is installed. The plugin and the
// schema change land together or not at all.
func TestBundleOverlayThatWidensIsRefusedEntirely(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}
			manifest := overlayManifest(t, dir)
			module := buildWasm(t, dir)

			widening := []byte("object: Contact\nnarrow_options:\n  kind: [customer, vendor, both, banana]\n")
			status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": packOverlayBundle(t, key, manifest, module, map[string][]byte{"Contact": widening}),
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("widening bundle = %d, want 422; body=%s", status, body)
			}
			if !strings.Contains(string(body), "banana") {
				t.Errorf("the refusal does not name the offending value: %s", body)
			}
			// Not half-installed: no plugin row, no overlay, no role.
			if _, listBody, _ := e.get("/api/v1/plugins"); strings.Contains(string(listBody), "com.acme.afternoon") {
				t.Errorf("a refused bundle installed the plugin anyway: %s", listBody)
			}
			if got := listOverlays(t, e); len(got) != 0 {
				t.Errorf("a refused bundle stored %d overlays: %v", len(got), got)
			}

			// Non-vacuity: the same bundle with a *narrowing* overlay installs.
			narrowing := []byte("object: Contact\nnarrow_options:\n  kind: [customer]\n")
			if status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": packOverlayBundle(t, key, manifest, module, map[string][]byte{"Contact": narrowing}),
			}); status != http.StatusCreated {
				t.Fatalf("narrowing bundle = %d, want 201; body=%s", status, body)
			}
		})
	}
}

// swapBundleEntry rewrites one file inside a packed bundle, leaving the
// signature document untouched — a bundle signed over contents it no longer
// carries, which is what tampering actually looks like.
func swapBundleEntry(t *testing.T, bundle []byte, name string, body []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = gz.Close() }()

	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", h.Name, err)
		}
		files[h.Name] = raw
	}
	if _, ok := files[name]; !ok {
		t.Fatalf("bundle carries no %s; it has %v", name, files)
	}
	files[name] = body

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// The overlay is inside the signature, so swapping one is the same refusal as
// swapping the module — which is the property the bundle format exists for.
func TestSwappedOverlayFailsTheSignature(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}
			manifest := overlayManifest(t, dir)
			module := buildWasm(t, dir)

			signed, err := plugins.Pack([]byte(manifest), module,
				map[string][]byte{"Contact": []byte(bundleOverlay)}, key.ID, key.Key)
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			tampered := swapBundleEntry(t, signed, "overlay.Contact.yaml",
				[]byte("object: Contact\nadd_fields:\n  - {name: acme_backdoor, type: text}\n"))

			status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": base64.StdEncoding.EncodeToString(tampered),
			})
			if status != http.StatusForbidden {
				t.Fatalf("swapped overlay = %d, want 403; body=%s", status, body)
			}
			if _, ok := metaFieldsFor(t, e, "Contact")["acme_backdoor"]; ok {
				t.Error("the swapped overlay's field reached the schema")
			}
		})
	}
}

//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/plugins"
)

// WP-3.2b: the supply-chain install path over the wired handler.
//
// Invariants: INV-T2/T3/T4 are the same ones `POST /api/v1/plugins` already
// carries — this proves the bundle route did not become a way around them —
// plus **INV-F5**, which the read-only document set introduces: a plugin may
// read an invoice and may never write one, because writes belong to the posting
// pipeline.

func TestBundleInstallRefusesWhatItCannotVerify(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}
			manifest := readFile(t, dir+"/manifest.yaml")
			module := buildWasm(t, dir)

			// Signed by a key this deployment does not trust.
			stranger, err := plugins.NewSigningKey(t.TempDir()+"/stranger.key", "stranger")
			if err != nil {
				t.Fatalf("NewSigningKey: %v", err)
			}
			strangerBundle, err := plugins.Pack([]byte(manifest), module, nil, stranger.ID, stranger.Key)
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": base64.StdEncoding.EncodeToString(strangerBundle),
			})
			if status != http.StatusForbidden {
				t.Fatalf("stranger's bundle = %d, want 403: %s", status, body)
			}
			if !strings.Contains(string(body), "untrusted") {
				t.Fatalf("the refusal does not say why: %s", body)
			}

			// Not a bundle at all.
			if status, body, _ = e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": base64.StdEncoding.EncodeToString([]byte("not an archive")),
			}); status != http.StatusUnprocessableEntity {
				t.Fatalf("garbage bundle = %d, want 422: %s", status, body)
			}

			// Nothing was installed by either attempt.
			_, listBody, _ := e.get("/api/v1/plugins")
			if strings.Contains(string(listBody), "com.acme.afternoon") {
				t.Fatalf("a refused bundle installed anyway: %s", listBody)
			}

			// Non-vacuity: the trusted publisher's bundle installs.
			installBundleOverHTTP(t, e, key, manifest, module)
		})
	}
}

// TestBundleInstallStillNeedsPermissionAndGrants: the signature is a gate in
// front of the install, not instead of it.
func TestBundleInstallStillNeedsPermissionAndGrants(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)

			dir := t.TempDir()
			if _, err := plugins.NewPlugin(dir, "go", "com.acme.afternoon"); err != nil {
				t.Fatalf("plugin new: %v", err)
			}
			bundle, err := plugins.Pack([]byte(readFile(t, dir+"/manifest.yaml")), buildWasm(t, dir), nil, key.ID, key.Key)
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			payload := map[string]any{"bundle": base64.StdEncoding.EncodeToString(bundle)}

			// No session at all.
			resp := e.raw(t, "POST", "/api/v1/plugins/bundle", "", "k1", payload)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated install = %d, want 401", resp.StatusCode)
			}

			// A session without `plugin:manage` — invoke is not enough to
			// install code, only to run what someone else approved.
			invoker := e.issueUser(t, map[string][]string{"plugin": {"invoke"}})
			resp2 := e.raw(t, "POST", "/api/v1/plugins/bundle", invoker, "k2", payload)
			_ = resp2.Body.Close()
			if resp2.StatusCode != http.StatusForbidden {
				t.Fatalf("install without plugin:manage = %d, want 403", resp2.StatusCode)
			}
		})
	}
}

// TestPluginMayReadAnInvoiceAndNeverWriteOne is INV-F5 at the plugin surface.
// The gateway serves no generic CRUD for Invoice precisely so nothing can
// create one outside invoicing's pipeline; the plugin host offers the same
// object for reading, and refuses a manifest that asks to write it — at
// install, by name, rather than at some later call nobody is watching.
func TestPluginMayReadAnInvoiceAndNeverWriteOne(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			key := trustedPublisher(t)
			e, _ := seedVault(t, db)
			module := buildWasm(t, "../../examples/plugins/commission-calc")

			writer := strings.Replace(
				readFile(t, "../../examples/plugins/commission-calc/manifest.yaml"),
				"{type: Invoice, access: read}", "{type: Invoice, access: write}", 1)
			bundle, err := plugins.Pack([]byte(writer), module, nil, key.ID, key.Key)
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			status, body, _ := e.post("/api/v1/plugins/bundle", map[string]any{
				"bundle": base64.StdEncoding.EncodeToString(bundle),
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("a plugin asking to write invoices installed: %d %s", status, body)
			}
			if !strings.Contains(string(body), "INV-F5") {
				t.Fatalf("the refusal does not name the invariant it enforces: %s", body)
			}

			// Non-vacuity: the same plugin asking to *read* invoices installs,
			// which is the whole distinction.
			installBundleOverHTTP(t, e, key,
				readFile(t, "../../examples/plugins/commission-calc/manifest.yaml"), module)
		})
	}
}

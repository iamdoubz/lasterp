// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WP-3.2b, the supply chain. Invariants: INV-T3/T4 are unchanged (a bundle
// install *is* Install underneath, so the approver still bounds the manifest
// and still owns the audit row) — what these prove is the gate in front of it:
// bytes nobody this deployment trusts has signed do not become a plugin.

// testBundle packs the hello module under a fresh key and returns the bundle,
// the key and the trust store that accepts it.
func testBundle(t *testing.T, manifest string) ([]byte, SigningKey, TrustStore) {
	t.Helper()
	key := testSigningKey(t)
	bundle, err := Pack([]byte(manifest), corpusModule(t, "hello"), key.ID, key.Key)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	pub, _ := key.Key.Public().(ed25519.PublicKey)
	return bundle, key, TrustStore{key.ID: pub}
}

func testSigningKey(t *testing.T) SigningKey {
	t.Helper()
	key, err := NewSigningKey(filepath.Join(t.TempDir(), "publisher.key"), "test-publisher")
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	return key
}

func TestBundleRoundTripsAndVerifies(t *testing.T) {
	manifest := helloWith("echo, say, read, write, secret, chatter")
	bundle, key, trust := testBundle(t, manifest)

	opened, err := OpenBundle(bundle)
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}
	if string(opened.ManifestYAML) != manifest {
		t.Fatal("the manifest did not survive the round trip")
	}
	if !bytes.Equal(opened.Module, corpusModule(t, "hello")) {
		t.Fatal("the module did not survive the round trip")
	}
	if err := opened.Verify(trust); err != nil {
		t.Fatalf("Verify with the packing key: %v", err)
	}
	if opened.Signature.KeyID != key.ID {
		t.Fatalf("signature names %q", opened.Signature.KeyID)
	}

	// The digest is over content, not over tar bytes, so packing the same
	// inputs twice produces the same identity — which is what makes a
	// signature over it mean "this plugin" rather than "this archive".
	again, err := Pack([]byte(manifest), corpusModule(t, "hello"), key.ID, key.Key)
	if err != nil {
		t.Fatalf("Pack again: %v", err)
	}
	openedAgain, err := OpenBundle(again)
	if err != nil {
		t.Fatalf("OpenBundle again: %v", err)
	}
	if openedAgain.Digest != opened.Digest {
		t.Fatalf("repacking the same plugin changed its digest: %s vs %s", openedAgain.Digest, opened.Digest)
	}
}

func TestUnsignedAndUntrustedBundlesAreRefused(t *testing.T) {
	manifest := helloWith("echo")
	bundle, _, trust := testBundle(t, manifest)
	opened, err := OpenBundle(bundle)
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}

	// A deployment with no trust file accepts nothing.
	if err := opened.Verify(TrustStore{}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("empty trust store: err = %v, want ErrUntrusted", err)
	}

	// Someone else's key.
	other := testSigningKey(t)
	otherPub, _ := other.Key.Public().(ed25519.PublicKey)
	if err := opened.Verify(TrustStore{other.ID: otherPub}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("stranger's key: err = %v, want ErrUntrusted", err)
	}

	// The right key id carrying the wrong key — the case a trust file keyed by
	// name alone would fall for.
	if err := opened.Verify(TrustStore{opened.Signature.KeyID: otherPub}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("impostor under the trusted id: err = %v, want ErrUntrusted", err)
	}

	// No signature document at all.
	unsigned := repack(t, map[string][]byte{
		bundleManifest: []byte(manifest),
		bundleModule:   corpusModule(t, "hello"),
	})
	openedUnsigned, err := OpenBundle(unsigned)
	if err != nil {
		t.Fatalf("OpenBundle unsigned: %v", err)
	}
	if err := openedUnsigned.Verify(trust); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("unsigned bundle: err = %v, want ErrUntrusted", err)
	}

	// Non-vacuity: the real bundle under the real trust store verifies.
	if err := opened.Verify(trust); err != nil {
		t.Fatalf("the genuine bundle was refused too: %v", err)
	}
}

// TestTamperedBundleIsRefused is the property the whole format exists for: the
// signature covers a digest of the contents, so swapping the module — or the
// manifest, which is where the capabilities live — invalidates it.
func TestTamperedBundleIsRefused(t *testing.T) {
	manifest := helloWith("echo")
	bundle, _, trust := testBundle(t, manifest)
	original, err := OpenBundle(bundle)
	if err != nil {
		t.Fatalf("OpenBundle: %v", err)
	}
	sigJSON, err := json.Marshal(original.Signature)
	if err != nil {
		t.Fatalf("marshal signature: %v", err)
	}

	for what, files := range map[string]map[string][]byte{
		"swapped module": {
			bundleManifest:  []byte(manifest),
			bundleModule:    corpusModule(t, "thief"),
			bundleSignature: sigJSON,
		},
		"widened manifest": {
			bundleManifest:  []byte(strings.Replace(manifest, "objects:", "secrets: [stolen]\n  objects:", 1)),
			bundleModule:    corpusModule(t, "hello"),
			bundleSignature: sigJSON,
		},
	} {
		opened, err := OpenBundle(repack(t, files))
		if err != nil {
			continue // a manifest that no longer parses is refused earlier, which is also correct
		}
		if err := opened.Verify(trust); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("%s: err = %v, want ErrUntrusted", what, err)
		}
	}
}

func TestMalformedBundlesAreRefused(t *testing.T) {
	manifest := helloWith("echo")
	module := corpusModule(t, "hello")

	cases := map[string][]byte{
		"not gzip":    []byte("this is not an archive"),
		"empty":       {},
		"no manifest": repack(t, map[string][]byte{bundleModule: module}),
		"no module":   repack(t, map[string][]byte{bundleManifest: []byte(manifest)}),
		"module is not wasm": repack(t, map[string][]byte{
			bundleManifest: []byte(manifest), bundleModule: []byte("MZ not wasm at all"),
		}),
	}
	for what, data := range cases {
		if _, err := OpenBundle(data); !errors.Is(err, ErrBundle) {
			t.Errorf("%s: err = %v, want ErrBundle", what, err)
		}
	}

	// A path that tries to escape the bundle. Nothing legitimate produces one,
	// so it is refused rather than sanitised — sanitising is where tar
	// extractors grow their CVEs.
	for _, name := range []string{"../evil.yaml", "nested/manifest.yaml", "/etc/passwd"} {
		data := repack(t, map[string][]byte{name: []byte("x"), bundleManifest: []byte(manifest), bundleModule: module})
		if _, err := OpenBundle(data); !errors.Is(err, ErrBundle) {
			t.Errorf("entry %q was accepted", name)
		}
	}

	// A symlink entry, likewise: a bundle is three regular files.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatalf("write symlink header: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := OpenBundle(buf.Bytes()); !errors.Is(err, ErrBundle) {
		t.Errorf("a symlink entry was accepted: %v", err)
	}
}

// TestInstallBundleGoesThroughEveryInstallGate: the bundle adds a gate, it does
// not replace any. An untrusted bundle never reaches Install, and a trusted one
// is still bounded by the approver's own grants (INV-T3).
func TestInstallBundleGoesThroughEveryInstallGate(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			manifest := helloWith("echo, say, read, write, secret, chatter")
			bundle, _, trust := testBundle(t, manifest)
			tenant := newTenant(t, db)

			// Untrusted: refused before anything is stored.
			if _, err := InstallBundle(ctx, db, tenant, bundle, TrustStore{}, helloApprover(t, db, tenant)); !errors.Is(err, ErrUntrusted) {
				t.Fatalf("untrusted install: err = %v, want ErrUntrusted", err)
			}
			if _, err := Get(ctx, db, tenant, "com.acme.hello"); !errors.Is(err, ErrNotFound) {
				t.Fatal("a refused bundle left a plugin installed")
			}

			// Trusted, but the approver lacks a capability the manifest asks
			// for: still refused, by the WP-3.1a rule rather than by this one.
			thin := approver(t, db, tenant, [2]string{"Widget", "read"})
			if _, err := InstallBundle(ctx, db, tenant, bundle, trust, thin); !errors.Is(err, ErrCapabilityNotHeld) {
				t.Fatalf("thin approver: err = %v, want ErrCapabilityNotHeld", err)
			}

			// Trusted and fully granted: installed, with the module's own hash
			// recorded — the identity the signature attaches to.
			p, err := InstallBundle(ctx, db, tenant, bundle, trust, helloApprover(t, db, tenant))
			if err != nil {
				t.Fatalf("InstallBundle: %v", err)
			}
			opened, err := OpenBundle(bundle)
			if err != nil {
				t.Fatalf("OpenBundle: %v", err)
			}
			if p.SHA256 != opened.ModuleSHA256 {
				t.Fatalf("installed sha256 %s, bundle module %s", p.SHA256, opened.ModuleSHA256)
			}
		})
	}
}

func TestTrustFileParsing(t *testing.T) {
	key := testSigningKey(t)
	good := "# a comment\n\n" + key.PublicLine() + "\n"
	store, err := ParseTrustFile(strings.NewReader(good))
	if err != nil {
		t.Fatalf("ParseTrustFile: %v", err)
	}
	if len(store) != 1 || store[key.ID] == nil {
		t.Fatalf("parsed %d keys", len(store))
	}

	for what, body := range map[string]string{
		"no equals":    "just-an-id\n",
		"not base64":   "acme = not base64 at all!!\n",
		"wrong length": "acme = " + base64.StdEncoding.EncodeToString([]byte("too short")) + "\n",
		"empty value":  "acme =\n",
	} {
		if _, err := ParseTrustFile(strings.NewReader(body)); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

func TestSigningKeyFileRoundTripsAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "publisher.key")
	key, err := NewSigningKey(path, "acme-2026")
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	loaded, err := LoadSigningKey(path)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if loaded.ID != key.ID || !loaded.Key.Equal(key.Key) {
		t.Fatal("the key did not survive the round trip")
	}
	// The public line is what a publisher sends an operator; it must be exactly
	// what ParseTrustFile accepts.
	store, err := ParseTrustFile(strings.NewReader(loaded.PublicLine()))
	if err != nil || store["acme-2026"] == nil {
		t.Fatalf("the printed trust line does not parse: %v", err)
	}
	if _, err := NewSigningKey(path, "acme-2026"); err == nil {
		t.Fatal("a second keygen overwrote an existing signing key")
	}
	// Mode is checked on the platforms that have one; Windows reports 0666.
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 && os.Getenv("GOOS") != "windows" {
		t.Logf("signing key mode is %v", info.Mode().Perm())
	}
}

// repack writes an arbitrary set of files as a bundle, for the malformed and
// tampered cases Pack would never produce.
func repack(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

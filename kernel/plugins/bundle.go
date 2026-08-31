// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Plugin bundles (WP-3.2b, ADR-007 "signed OCI-style bundles").
//
// **OCI-style, not OCI** (WP-3.2-decisions.md §3): a gzipped tar carrying
// `manifest.yaml`, `plugin.wasm` and a detached `signature.json`, served from
// any HTTP host. Taken literally, "OCI" means a registry client, a media-type
// scheme and probably a signing dependency, for a v1 whose job is "one file
// that carries a plugin and proves who built it". This is stdlib only —
// archive/tar, compress/gzip, crypto/ed25519 — which is the right shape for a
// supply-chain path: the thing that decides what code runs should not itself
// widen the dependency surface.
//
// Identity is the digest WP-3.1a already records and re-checks on every load.
// The signature attaches to that rather than inventing a second identity, so a
// bundle whose module was swapped fails the same check that a tampered
// installed row fails.

// Bundle limits. A plugin is a couple of megabytes of WASM; anything larger is
// an attack on the server's memory before it is ever a sandboxed one.
const (
	// MaxBundleBytes caps the compressed bundle.
	MaxBundleBytes = 40 << 20
	// maxBundleEntries caps how many files a bundle may hold. Three are
	// expected; the cap is what stops a tar with a million empty headers.
	maxBundleEntries = 16
	// maxManifestBytes caps the manifest document.
	maxManifestBytes = 1 << 20

	bundleManifest  = "manifest.yaml"
	bundleModule    = "plugin.wasm"
	bundleSignature = "signature.json"

	// SignatureAlgo is the only algorithm this host verifies. One algorithm,
	// because a negotiable one is a downgrade attack with extra steps.
	SignatureAlgo = "ed25519"
)

// ErrBundle is every malformed-bundle refusal.
var ErrBundle = errors.New("plugins: bundle refused")

// ErrUntrusted means the bundle is unsigned, or signed by a key this
// deployment does not trust. It is deliberately one error: "unsigned" and
// "signed by a stranger" are the same answer.
var ErrUntrusted = errors.New("plugins: bundle is not signed by a trusted publisher")

// Signature is the detached signature document a bundle carries.
type Signature struct {
	KeyID string `json:"key_id"`
	Algo  string `json:"algo"`
	// Digest is the bundle's content digest, hex-encoded — what was signed.
	Digest string `json:"digest"`
	// Sig is the base64 ed25519 signature over the digest's *bytes*.
	Sig string `json:"signature"`
}

// Bundle is an opened bundle: its documents, its computed digest, and whatever
// signature it carried. Opening proves the digest; Verify proves the signer.
type Bundle struct {
	ManifestYAML []byte
	Module       []byte
	Signature    Signature
	// Digest is computed from the contents, never read from the file. A digest
	// a bundle asserts about itself is not evidence.
	Digest string
	// ModuleSHA256 is the module's own hash — the identity WP-3.1a records on
	// the installed row and re-checks on every load.
	ModuleSHA256 string
}

// Pack builds a signed bundle. It is the publishing half, used by
// `lasterp plugin pack` and by the tests; the server never packs.
func Pack(manifestYAML, module []byte, keyID string, priv ed25519.PrivateKey) ([]byte, error) {
	if _, err := ParseManifest(manifestYAML); err != nil {
		return nil, err
	}
	if !isWASM(module) {
		return nil, fmt.Errorf("%w: module is not a WebAssembly binary", ErrBundle)
	}
	if keyID == "" || len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: a key id and an ed25519 private key are required", ErrBundle)
	}

	files := map[string][]byte{bundleManifest: manifestYAML, bundleModule: module}
	digest := contentDigest(files)
	sig := Signature{
		KeyID:  keyID,
		Algo:   SignatureAlgo,
		Digest: digest,
		Sig:    base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(digest))),
	}
	sigJSON, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return nil, err
	}
	files[bundleSignature] = sigJSON

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedNames(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenBundle reads a bundle and recomputes its digest. It does not decide
// whether the signer is trusted — that is Verify, and keeping them apart is
// what lets `lasterp plugin pack` inspect its own output without a trust store.
func OpenBundle(data []byte) (*Bundle, error) {
	if len(data) == 0 || len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("%w: bundle is %d bytes (limit %d)", ErrBundle, len(data), MaxBundleBytes)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip archive", ErrBundle)
	}
	defer func() { _ = gz.Close() }()

	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for i := 0; ; i++ {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: unreadable archive", ErrBundle)
		}
		if i >= maxBundleEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrBundle, maxBundleEntries)
		}
		// A bundle is three flat files. Anything with a directory component, a
		// traversal, an absolute path or a type other than "regular file" is
		// refused rather than sanitised: nothing legitimate produces one, and
		// sanitising is where tar extractors grow their CVEs.
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: %q is not a regular file", ErrBundle, header.Name)
		}
		name := header.Name
		if name != path.Base(name) || name == "." || name == ".." || strings.ContainsRune(name, '\\') {
			return nil, fmt.Errorf("%w: %q is not a plain file name", ErrBundle, header.Name)
		}
		// The size cap is applied to the *decompressed* stream, so a gzip bomb
		// is refused as it inflates rather than after.
		body, err := io.ReadAll(io.LimitReader(tr, MaxBundleBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: unreadable entry %q", ErrBundle, name)
		}
		if len(body) > MaxBundleBytes {
			return nil, fmt.Errorf("%w: entry %q is over the size limit", ErrBundle, name)
		}
		files[name] = body
	}

	b := &Bundle{ManifestYAML: files[bundleManifest], Module: files[bundleModule]}
	if len(b.ManifestYAML) == 0 {
		return nil, fmt.Errorf("%w: no %s", ErrBundle, bundleManifest)
	}
	if len(b.ManifestYAML) > maxManifestBytes {
		return nil, fmt.Errorf("%w: %s is over the size limit", ErrBundle, bundleManifest)
	}
	if len(b.Module) == 0 {
		return nil, fmt.Errorf("%w: no %s", ErrBundle, bundleModule)
	}
	if !isWASM(b.Module) {
		return nil, fmt.Errorf("%w: %s is not a WebAssembly binary", ErrBundle, bundleModule)
	}
	if raw, ok := files[bundleSignature]; ok {
		if err := json.Unmarshal(raw, &b.Signature); err != nil {
			return nil, fmt.Errorf("%w: %s is not JSON", ErrBundle, bundleSignature)
		}
	}

	// Computed from what is actually here, never read from the document.
	b.Digest = contentDigest(map[string][]byte{
		bundleManifest: b.ManifestYAML,
		bundleModule:   b.Module,
	})
	sum := sha256.Sum256(b.Module)
	b.ModuleSHA256 = hex.EncodeToString(sum[:])
	return b, nil
}

// Verify checks the signature against the deployment's trusted publishers.
func (b *Bundle) Verify(trust TrustStore) error {
	if len(trust) == 0 {
		return fmt.Errorf("%w: this deployment has no publisher trust file (%s)", ErrUntrusted, EnvTrustFile)
	}
	if b.Signature.Sig == "" || b.Signature.KeyID == "" {
		return fmt.Errorf("%w: the bundle carries no signature", ErrUntrusted)
	}
	if b.Signature.Algo != SignatureAlgo {
		return fmt.Errorf("%w: signature algorithm %q is not %s", ErrUntrusted, b.Signature.Algo, SignatureAlgo)
	}
	// The signature covers the digest the *contents* produce, not the one the
	// document claims. A bundle that names someone else's digest is signed over
	// bytes it does not carry.
	if b.Signature.Digest != b.Digest {
		return fmt.Errorf("%w: the signature covers a different digest", ErrUntrusted)
	}
	pub, ok := trust[b.Signature.KeyID]
	if !ok {
		return fmt.Errorf("%w: key %q is not in the trust file", ErrUntrusted, b.Signature.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(b.Signature.Sig)
	if err != nil {
		return fmt.Errorf("%w: the signature is not base64", ErrUntrusted)
	}
	if !ed25519.Verify(pub, []byte(b.Digest), sig) {
		return fmt.Errorf("%w: the signature does not verify", ErrUntrusted)
	}
	return nil
}

// InstallBundle verifies a bundle and installs what it carries.
//
// Every gate WP-3.1a put on `Install` still applies — it *is* Install
// underneath, so the approver's grants still bound the manifest (INV-T3), the
// install is still attributable (INV-T4), and the module's hash is still
// recorded. The bundle adds one gate in front: bytes nobody this deployment
// trusts has signed do not get to be a plugin.
func InstallBundle(ctx context.Context, db *storage.DB, tenant tenancy.ID, data []byte, trust TrustStore, approver authz.Actor) (*Installed, error) {
	b, err := OpenBundle(data)
	if err != nil {
		return nil, err
	}
	if err := b.Verify(trust); err != nil {
		return nil, err
	}
	return Install(ctx, db, tenant, b.ManifestYAML, b.Module, approver)
}

// contentDigest is the bundle's identity: SHA-256 over a canonical list of
// "name\x00file-hash" lines, sorted by name.
//
// A digest over the *tar bytes* would change with a timestamp, a file order or
// a gzip level, which would make "the same plugin" produce a different identity
// on every build. This one is stable across repacking, which is what a
// signature over it needs to mean.
func contentDigest(files map[string][]byte) string {
	h := sha256.New()
	for _, name := range sortedNames(files) {
		if name == bundleSignature {
			continue // a signature cannot cover itself
		}
		sum := sha256.Sum256(files[name])
		// hash.Hash never returns a write error, which is why a digest can be
		// computed without checking one.
		_, _ = fmt.Fprintf(h, "%s\x00%s\n", name, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// --- the trust store ---

// EnvTrustFile names the file listing publisher keys this deployment trusts.
const EnvTrustFile = "LASTERP_PLUGIN_TRUST_FILE"

// TrustStore is key id → public key.
//
// It is a **deployment** file, not a per-tenant table (WP-3.2-decisions.md §3).
// Per-tenant publisher trust is what a marketplace needs, and there is no
// marketplace: every install today is an operator handing a bundle to their own
// deployment, so "who may publish plugins here" is an operator fact and lives
// where the vault's key file lives.
type TrustStore map[string]ed25519.PublicKey

// LoadTrustStore reads EnvTrustFile. An unset variable is not an error — it is
// a deployment that installs modules directly and never bundles, and the empty
// store refuses every bundle by construction.
func LoadTrustStore() (TrustStore, error) {
	path := os.Getenv(EnvTrustFile)
	if path == "" {
		return TrustStore{}, nil
	}
	// #nosec G304,G703 -- the path is this deployment's own configuration, read
	// once at boot; it is not attacker-controlled input.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugins: open trust file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ParseTrustFile(f)
}

// ParseTrustFile reads `key_id = base64 public key` lines, `#` comments and
// blanks ignored — the same shape as the vault's key file, deliberately, so an
// operator learns one format.
func ParseTrustFile(r io.Reader) (TrustStore, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("plugins: read trust file: %w", err)
	}
	store := TrustStore{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, value, ok := strings.Cut(line, "=")
		id, value = strings.TrimSpace(id), strings.TrimSpace(value)
		if !ok || id == "" || value == "" {
			return nil, fmt.Errorf("plugins: trust file line %d is not `key_id = base64 key`", i+1)
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("plugins: trust file line %d: %q is not a base64 ed25519 public key", i+1, id)
		}
		store[id] = ed25519.PublicKey(key)
	}
	return store, nil
}

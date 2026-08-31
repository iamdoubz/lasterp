// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The registry (WP-3.2b, ADR-007 "a public registry plus private tenant
// registries").
//
// A registry is a directory of bundles with an `index.json` beside them, served
// over HTTPS by anything — a bucket, a static site, a company file server.
// There is no registry *protocol* to implement and no server to run, which is
// the point: the security of an install comes from the signature (bundle.go),
// not from where the bytes were fetched, so the transport can be as boring as
// possible.
//
// This half runs on the operator's machine, inside `lasterp plugin install`.
// The server never fetches a bundle: an install is an approval decision that
// must be attributable to a person and bounded by *that person's* grants
// (WP-3.1-decisions.md §6), so the CLI downloads, and then posts it to the
// authenticated API as that person.

// maxIndexBytes caps a registry index. It is a list of a few hundred entries at
// most; anything larger is a server having a bad day, or a hostile one.
const maxIndexBytes = 4 << 20

// Index is a registry's catalogue.
type Index struct {
	Plugins []IndexEntry `json:"plugins"`
}

// IndexEntry is one published version.
type IndexEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Bundle is the file name or URL of the bundle, resolved against the
	// index's own URL when relative.
	Bundle string `json:"bundle"`
	// Digest is the bundle's content digest, hex — the same digest bundle.go
	// computes. Optional, and checked when present: it lets a mirror be caught
	// serving different bytes than the index it publishes, without anyone
	// needing the publisher's key.
	Digest string `json:"digest"`
}

// Resolve picks the entry for a reference of the form `id` or `id@version`.
// With no version, the highest one wins — "install the newest" is what an
// operator typing a bare id means.
func (idx Index) Resolve(ref string) (IndexEntry, error) {
	id, want, hasVersion := strings.Cut(ref, "@")
	var best IndexEntry
	var bestV version
	found := false
	for _, e := range idx.Plugins {
		if e.ID != id {
			continue
		}
		if hasVersion {
			if e.Version == want {
				return e, nil
			}
			continue
		}
		v, err := parseVersion(e.Version)
		if err != nil {
			continue // an unparseable version cannot be "the newest"
		}
		if !found || compare(v, bestV) > 0 {
			best, bestV, found = e, v, true
		}
	}
	if !found {
		if hasVersion {
			return IndexEntry{}, fmt.Errorf("plugins: the registry has no %s at version %s", id, want)
		}
		return IndexEntry{}, fmt.Errorf("plugins: the registry has no %s", id)
	}
	return best, nil
}

// FetchIndex reads a registry's index.json.
func FetchIndex(ctx context.Context, client *http.Client, registryURL string) (Index, error) {
	body, err := fetchURL(ctx, client, strings.TrimSuffix(registryURL, "/")+"/index.json", maxIndexBytes)
	if err != nil {
		return Index{}, err
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return Index{}, fmt.Errorf("plugins: registry index is not JSON: %w", err)
	}
	return idx, nil
}

// FetchBundle downloads one entry's bundle and checks it against the index's
// digest when the index published one.
func FetchBundle(ctx context.Context, client *http.Client, registryURL string, e IndexEntry) ([]byte, error) {
	url := e.Bundle
	if !strings.Contains(url, "://") {
		url = strings.TrimSuffix(registryURL, "/") + "/" + strings.TrimPrefix(url, "/")
	}
	data, err := fetchURL(ctx, client, url, MaxBundleBytes)
	if err != nil {
		return nil, err
	}
	if e.Digest != "" {
		b, err := OpenBundle(data)
		if err != nil {
			return nil, err
		}
		if b.Digest != e.Digest {
			return nil, fmt.Errorf("%w: the registry index lists digest %s and the bundle is %s",
				ErrBundle, e.Digest, b.Digest)
		}
	}
	return data, nil
}

func fetchURL(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plugins: fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugins: fetch %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("plugins: read %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("plugins: %s is over the %d-byte limit", url, limit)
	}
	return body, nil
}

// --- the publisher's signing key ---

// SigningKey is a publisher's private key and the id it publishes under.
type SigningKey struct {
	ID  string
	Key ed25519.PrivateKey
}

// NewSigningKey generates a key and writes it to path, mode 0600, refusing to
// overwrite — the same rules as the vault's key file, for the same reason: a
// key file silently replaced is a key file nobody can verify against
// afterwards.
func NewSigningKey(path, keyID string) (SigningKey, error) {
	if keyID == "" {
		return SigningKey{}, fmt.Errorf("plugins: a key id is required")
	}
	if _, err := os.Stat(path); err == nil {
		return SigningKey{}, fmt.Errorf("plugins: %s already exists; refusing to overwrite a signing key", path)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return SigningKey{}, err
	}
	doc := fmt.Sprintf(`# LastERP plugin signing key — keep this file secret.
# The matching trust-file line for a deployment that should accept your bundles:
#
#   %s = %s
#
id = %s
private = %s
`, keyID, base64.StdEncoding.EncodeToString(pub), keyID, base64.StdEncoding.EncodeToString(priv))
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return SigningKey{}, fmt.Errorf("plugins: write signing key: %w", err)
	}
	return SigningKey{ID: keyID, Key: priv}, nil
}

// LoadSigningKey reads a key file written by NewSigningKey.
func LoadSigningKey(path string) (SigningKey, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is the publisher's own key file
	if err != nil {
		return SigningKey{}, fmt.Errorf("plugins: read signing key: %w", err)
	}
	var key SigningKey
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch name {
		case "id":
			key.ID = value
		case "private":
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(decoded) != ed25519.PrivateKeySize {
				return SigningKey{}, fmt.Errorf("plugins: %s: private is not a base64 ed25519 private key", path)
			}
			key.Key = ed25519.PrivateKey(decoded)
		}
	}
	if key.ID == "" || key.Key == nil {
		return SigningKey{}, fmt.Errorf("plugins: %s is missing `id` or `private`", path)
	}
	return key, nil
}

// PublicLine renders the trust-file line for this key, which is what a
// publisher sends an operator.
func (k SigningKey) PublicLine() string {
	pub, ok := k.Key.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return k.ID + " = " + base64.StdEncoding.EncodeToString(pub)
}

// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The key file is the deployment's root of trust for the vault (INV-K1), so
// every way of getting it wrong has to be an error rather than a silently
// weaker vault: a missing key is data nobody can recover, and a mis-sized one
// would be a shorter cipher key than the code claims.

func writeKeyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lasterp.keys")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func b64Key(b byte) string {
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = b
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestKeyFileRoundTrips(t *testing.T) {
	path := writeKeyFile(t, strings.Join([]string{
		"# a comment",
		"",
		"current = 2026-08-a",
		"2026-08-a = " + b64Key(1),
		"2026-07-a = " + b64Key(2), // retired, still readable
	}, "\n"))

	src, err := LoadKeyFile(path)
	if err != nil {
		t.Fatalf("LoadKeyFile: %v", err)
	}
	if src.KeyID() != "2026-08-a" {
		t.Errorf("KeyID = %q", src.KeyID())
	}

	dek := mustKey(t)
	wrapped, keyID, err := src.Wrap(dek, []byte("aad"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if keyID != "2026-08-a" {
		t.Errorf("Wrap used key %q, want the current one", keyID)
	}
	got, err := src.Unwrap(wrapped, keyID, []byte("aad"))
	if err != nil || string(got) != string(dek) {
		t.Fatalf("Unwrap = %v, %v", got, err)
	}
	if _, err := src.Unwrap(wrapped, "2026-07-a", []byte("aad")); err == nil {
		t.Error("a data key unwrapped under the wrong key id")
	}
	if _, err := src.Unwrap(wrapped, "never-existed", []byte("aad")); err == nil ||
		!strings.Contains(err.Error(), "never-existed") {
		t.Errorf("unwrapping under an absent key = %v; it must name the key", err)
	}
}

func TestKeyFileRejectsEveryMalformedShape(t *testing.T) {
	cases := map[string]string{
		"no current":           "k1 = " + b64Key(1),
		"current not present":  "current = k2\nk1 = " + b64Key(1),
		"not base64":           "current = k1\nk1 = not-base-64!!",
		"wrong length":         "current = k1\nk1 = " + base64.StdEncoding.EncodeToString([]byte("short")),
		"no equals sign":       "current = k1\nk1 " + b64Key(1),
		"empty file":           "",
		"comment-only current": "# current = k1\nk1 = " + b64Key(1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKeyFile(writeKeyFile(t, body)); err == nil {
				t.Error("accepted a malformed key file")
			}
		})
	}

	if _, err := LoadKeyFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("accepted a key file that does not exist")
	}
}

func TestLoadKeySourceIsNilWithoutTheEnvVar(t *testing.T) {
	t.Setenv(EnvKeyFile, "")
	src, err := LoadKeySource()
	if !errors.Is(err, ErrNoKeySource) {
		t.Fatalf("LoadKeySource = %v, want ErrNoKeySource", err)
	}
	// Nil as an interface, not a non-nil interface holding a nil pointer —
	// callers guard on `src == nil` and the typed-nil form slips past it.
	if src != nil {
		t.Error("LoadKeySource returned a non-nil KeySource alongside ErrNoKeySource")
	}

	t.Setenv(EnvKeyFile, filepath.Join(t.TempDir(), "absent"))
	if src, err := LoadKeySource(); err == nil || src != nil {
		t.Errorf("LoadKeySource with an unreadable file = %v, %v", src, err)
	}
}

func TestNewKeyFileRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lasterp.keys")
	if err := NewKeyFile(path, "k1"); err != nil {
		t.Fatalf("NewKeyFile: %v", err)
	}
	src, err := LoadKeyFile(path)
	if err != nil {
		t.Fatalf("LoadKeyFile: %v", err)
	}
	if src.KeyID() != "k1" {
		t.Errorf("KeyID = %q, want k1", src.KeyID())
	}

	// A key file replaced by accident is every secret in the deployment lost
	// at once, so creating one is never an overwrite.
	if err := NewKeyFile(path, "k2"); err == nil {
		t.Fatal("NewKeyFile overwrote an existing key file")
	}
	again, err := LoadKeyFile(path)
	if err != nil || again.KeyID() != "k1" {
		t.Fatalf("the original key file did not survive: %v, %v", again, err)
	}
}

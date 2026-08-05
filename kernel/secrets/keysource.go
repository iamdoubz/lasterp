// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// EnvKeyFile names the file FileKeySource reads. Unset means the deployment
// has no key source and the vault is unavailable — deliberately, rather than
// generating a key on first use: a silently generated KEK is discovered at
// restore time by someone holding a database backup and no key
// (WP-3.0-decisions.md §3).
const EnvKeyFile = "LASTERP_SECRETS_KEYFILE"

// ErrNoKeySource is returned by every vault operation when the deployment has
// not configured a key source.
var ErrNoKeySource = errors.New("secrets: no key source configured (" + EnvKeyFile + " is unset)")

// KeySource holds the deployment's key-encryption keys (KEKs). It wraps and
// unwraps per-secret data keys and never sees a secret's plaintext.
//
// It is this small on purpose: docs/08 names a KMS for cloud and a key file
// for self-host, and only the file implementation ships (§2). A KMS source is
// three methods against whatever SDK that deployment already has — the reason
// it is not here is that shipping a cloud SDK for a deployment shape nobody
// runs yet is a new runtime dependency for nothing.
type KeySource interface {
	// KeyID names the key new writes are wrapped with. Rotation is defined as
	// "every row whose key_id is not this one".
	KeyID() string
	// Wrap seals a data key with the current KEK, returning the sealed bytes
	// and the id of the key that sealed them.
	Wrap(dek, aad []byte) (wrapped []byte, keyID string, err error)
	// Unwrap opens a data key sealed by the named key. A key id the source no
	// longer holds is an error, never a silent miss.
	Unwrap(wrapped []byte, keyID string, aad []byte) (dek []byte, err error)
}

// FileKeySource reads KEKs from a file, the self-host shape of docs/08's
// "keys in KMS/age file".
//
// Format — one `name = value` per line, `#` comments, blank lines ignored:
//
//	current = 2026-08-a
//	2026-08-a = <base64 of 32 random bytes>
//	2026-07-a = <base64 of 32 random bytes>   # kept until rotation drains it
//
// Retired keys stay in the file until `lasterp secrets rotate` has moved every
// row off them; removing one early is what makes a secret unrecoverable.
//
// Not the age file format, and no filippo.io/age: age's value is its
// recipient model for sharing files between people, and a deployment KEK is
// read by one process and shared with nobody (§2).
type FileKeySource struct {
	current string
	keys    map[string][]byte
}

// LoadKeySource returns the key source named by EnvKeyFile, or ErrNoKeySource
// when it is unset. It is the one place the server decides whether it has a
// vault at all.
func LoadKeySource() (KeySource, error) {
	path := os.Getenv(EnvKeyFile)
	if path == "" {
		return nil, ErrNoKeySource
	}
	// Returned as an explicit nil interface on failure rather than
	// `return LoadKeyFile(path)`, which would hand back a non-nil interface
	// holding a nil *FileKeySource — the one shape a `src == nil` guard misses.
	src, err := LoadKeyFile(path)
	if err != nil {
		return nil, err
	}
	return src, nil
}

// LoadKeyFile parses a key file. Every failure is fatal to the vault rather
// than degrading to fewer keys: a source that silently drops the key one row
// needs turns a decryption failure into a mystery.
func LoadKeyFile(path string) (*FileKeySource, error) {
	// #nosec G703,G304 -- the key file path is deployment configuration (an env
	// var or a CLI flag set by the operator), never request data. A vault whose
	// key file location could not be chosen would be a vault with a hardcoded
	// key file.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: key file: %w", err)
	}
	// Windows file modes do not carry POSIX bits (os.Stat reports 0666 for an
	// ordinary file), so the check would be a guaranteed false positive there.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secrets: key file %s is mode %#o; it must not be readable by group or other", path, info.Mode().Perm())
	}

	// #nosec G304,G703 -- operator-configured path, as above.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: key file: %w", err)
	}
	defer func() { _ = f.Close() }()

	src := &FileKeySource{keys: map[string][]byte{}}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("secrets: key file %s line %d: want `name = value`", path, line)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "current" {
			src.current = value
			continue
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("secrets: key file %s line %d: key %q is not base64: %w", path, line, name, err)
		}
		if len(key) != keyLen {
			return nil, fmt.Errorf("secrets: key file %s line %d: key %q is %d bytes, want %d", path, line, name, len(key), keyLen)
		}
		src.keys[name] = key
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("secrets: key file %s: %w", path, err)
	}
	if src.current == "" {
		return nil, fmt.Errorf("secrets: key file %s names no `current` key", path)
	}
	if _, ok := src.keys[src.current]; !ok {
		return nil, fmt.Errorf("secrets: key file %s names current key %q, which it does not contain", path, src.current)
	}
	return src, nil
}

func (s *FileKeySource) KeyID() string { return s.current }

func (s *FileKeySource) Wrap(dek, aad []byte) ([]byte, string, error) {
	wrapped, err := seal(s.keys[s.current], dek, aad)
	if err != nil {
		return nil, "", err
	}
	return wrapped, s.current, nil
}

func (s *FileKeySource) Unwrap(wrapped []byte, keyID string, aad []byte) ([]byte, error) {
	key, ok := s.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("secrets: key %q is not in the key file; it must stay there until rotation has moved every secret off it", keyID)
	}
	return open(key, wrapped, aad)
}

// keyLen is 32 bytes: AES-256, for both the KEKs and the per-secret data keys.
const keyLen = 32

// randomKey mints a per-secret data key.
func randomKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate data key: %w", err)
	}
	return key, nil
}

// seal encrypts with AES-256-GCM, prepending the nonce. aad binds the
// ciphertext to where it lives, so a row copied into another tenant, another
// name, or (for a wrapped key) another key id fails to open rather than
// decrypting into the wrong place.
func seal(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("secrets: sealed value is too short to contain a nonce")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		// Deliberately not wrapped with the cipher's message: the caller gets
		// "this did not open", not a hint about which part failed.
		return nil, errors.New("secrets: could not decrypt (wrong key, or the value does not belong where it was found)")
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secrets: key is %d bytes, want %d", len(key), keyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	return gcm, nil
}

// NewKeyFile writes a key file containing one fresh key, for `lasterp secrets
// init`. It refuses to overwrite: a key file replaced by accident is every
// secret in the deployment lost at once.
func NewKeyFile(path, keyID string) error {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("secrets: generate key: %w", err)
	}
	body := fmt.Sprintf("# LastERP secrets key file. Back this up; a lost key is unrecoverable data.\ncurrent = %s\n%s = %s\n",
		keyID, keyID, base64.StdEncoding.EncodeToString(key))
	// #nosec G304 -- operator-configured path (`lasterp secrets init -keyfile`).
	// O_EXCL is the guard that matters here: this never overwrites.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("secrets: create key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(body); err != nil {
		return fmt.Errorf("secrets: write key file: %w", err)
	}
	return nil
}

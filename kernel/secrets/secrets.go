// SPDX-License-Identifier: AGPL-3.0-only

// Package secrets is the kernel vault (WP-3.0, docs/08 §Data protection): the
// tenant-scoped store for connector, plugin and provider credentials.
//
// Every value is sealed with AES-256-GCM under a data key generated for that
// one secret, and the data key is stored wrapped by the deployment's key
// source. Nothing in the database, the logs, the event store, the change feed
// or a client replica holds a secret in the clear — that is INV-K1, and the
// per-secret data key is what makes rotation a re-wrap of one short column
// rather than a decrypt-and-re-encrypt of every payload (WP-3.0-decisions.md
// §1).
//
// There is deliberately no HTTP path that returns a value (§4). A secret is
// read by the server on the tenant's behalf, through Get, by a caller that
// names itself.
package secrets

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrNotFound is returned when the tenant has no secret by that name. It is
// deliberately the same error whether the name has never existed or was
// deleted — a vault that distinguishes them enumerates itself.
var ErrNotFound = errors.New("secrets: no such secret")

// ErrForbidden is returned when a reader asks for a secret its grants do not
// cover. It carries no indication of whether the secret exists.
var ErrForbidden = errors.New("secrets: reader is not granted this secret")

// Value is secret plaintext. It renders as [REDACTED] through fmt and
// encoding/json, so the ordinary ways a value ends up in a log line or an
// error body — %v, %s, a struct marshalled to JSON — cannot leak it. Reading
// the bytes takes an explicit conversion, which is a thing a reviewer can see.
type Value []byte

func (Value) String() string { return "[REDACTED]" }

func (Value) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// Reader identifies who is reading a secret: a first-party module, a plugin, an
// automation, an agent. Get refuses an empty one, so no read is anonymous
// (INV-T4).
type Reader struct {
	Kind string // "module", "plugin", "automation", "agent"
	ID   string // "oidc", "com.acme.commission-calc", "notify-ops", …
}

func (r Reader) String() string { return r.Kind + ":" + r.ID }

func (r Reader) valid() bool { return r.Kind != "" && r.ID != "" }

// Grants reports whether a reader may read the named secret. It is the seam
// WP-3.1 fills with the plugin manifest's `secrets:` list (docs/05) — the
// capability check that makes INV-X1 true for the vault. Until a manifest
// exists there is nothing to check, so first-party callers pass AllowAll.
type Grants func(reader Reader, name string) bool

// AllowAll is the grants function for callers compiled into the server, which
// are inside the trust boundary the plugin host draws. It is not a default:
// every call site names it, so `grep AllowAll` lists everything that can read
// a secret without a manifest.
func AllowAll(Reader, string) bool { return true }

// Metadata is what the management API may see: everything about a secret
// except the secret.
type Metadata struct {
	Name        string
	Description string
	KeyID       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	UpdatedBy   string
}

// nameRE bounds secret names at the trust boundary: they arrive as a path
// segment and become an audit record_id and a plugin manifest reference.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidName reports whether s is an acceptable secret name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Put stores (or replaces) a secret. The caller has already authorized the
// write; actor is the principal it authorized, recorded so the change is
// attributable (INV-T2/T4 are decided one line apart in internal/app/secrets.go,
// the same shape as devices).
//
// Replacing keeps created_at and mints a fresh data key and nonce, so two
// writes of the same value produce different ciphertext.
func Put(ctx context.Context, db *storage.DB, src KeySource, tenant tenancy.ID, name, description string, value []byte, actor string) error {
	if src == nil {
		return ErrNoKeySource
	}
	if tenant == "" || actor == "" {
		return errors.New("secrets: tenant and actor are required")
	}
	if !ValidName(name) {
		return fmt.Errorf("secrets: %q is not a valid secret name", name)
	}
	if len(value) == 0 {
		return errors.New("secrets: an empty value is not a secret")
	}

	dek, err := randomKey()
	if err != nil {
		return err
	}
	ciphertext, err := seal(dek, value, valueAAD(tenant, name))
	if err != nil {
		return err
	}
	wrapped, keyID, err := src.Wrap(dek, dekAAD(tenant, name))
	if err != nil {
		return fmt.Errorf("secrets: wrap data key: %w", err)
	}

	now := time.Now().UTC()
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT name FROM secrets WHERE tenant_id = ? AND name = ?`), string(tenant), name).Scan(&existing)
		action := "update"
		if errors.Is(err, sql.ErrNoRows) {
			action = "create"
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO secrets (tenant_id, name, description, key_id, wrapped_dek, ciphertext,
			                     created_at, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, name) DO UPDATE SET
				description = excluded.description,
				key_id      = excluded.key_id,
				wrapped_dek = excluded.wrapped_dek,
				ciphertext  = excluded.ciphertext,
				updated_at  = excluded.updated_at,
				updated_by  = excluded.updated_by`),
			string(tenant), name, description, keyID, encode(wrapped), encode(ciphertext), now, now, actor); err != nil {
			return err
		}
		return recordAudit(ctx, tx, db, tenant, name, action, actor, nil)
	})
	if err != nil {
		return fmt.Errorf("secrets: put %q: %w", name, err)
	}
	return nil
}

// Get opens a secret for a named reader, recording the read.
//
// The audit row is written in the same transaction as the read, so a secret
// that was read is a secret whose reading is attributable and vice versa
// (INV-T4). A read that fails authorization is refused before the row is
// fetched and is not recorded as a read — it did not happen.
func Get(ctx context.Context, db *storage.DB, src KeySource, tenant tenancy.ID, name string, reader Reader, grants Grants) (Value, error) {
	if src == nil {
		return nil, ErrNoKeySource
	}
	if tenant == "" {
		return nil, errors.New("secrets: tenant is required")
	}
	if !reader.valid() {
		return nil, errors.New("secrets: a named reader is required to read a secret (INV-T4)")
	}
	if grants == nil || !grants(reader, name) {
		return nil, ErrForbidden
	}

	var value Value
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var keyID, wrappedB64, ciphertextB64 string
		err := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT key_id, wrapped_dek, ciphertext FROM secrets WHERE tenant_id = ? AND name = ?`),
			string(tenant), name).Scan(&keyID, &wrappedB64, &ciphertextB64)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		wrapped, err := decode(wrappedB64)
		if err != nil {
			return err
		}
		ciphertext, err := decode(ciphertextB64)
		if err != nil {
			return err
		}
		dek, err := src.Unwrap(wrapped, keyID, dekAAD(tenant, name))
		if err != nil {
			return err
		}
		plaintext, err := open(dek, ciphertext, valueAAD(tenant, name))
		if err != nil {
			return err
		}
		value = plaintext
		return recordAudit(ctx, tx, db, tenant, name, "read", reader.String(), map[string]any{"reader": reader.String()})
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("secrets: get %q: %w", name, err)
	}
	return value, nil
}

// List returns the tenant's secrets without their values.
func List(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Metadata, error) {
	if tenant == "" {
		return nil, errors.New("secrets: tenant is required")
	}
	var out []Metadata
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT name, description, key_id, created_at, updated_at, updated_by
			FROM secrets WHERE tenant_id = ? ORDER BY name`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []Metadata
		for rows.Next() {
			var m Metadata
			var createdAt, updatedAt storage.Time
			if err := rows.Scan(&m.Name, &m.Description, &m.KeyID, &createdAt, &updatedAt, &m.UpdatedBy); err != nil {
				return err
			}
			m.CreatedAt, m.UpdatedAt = createdAt.Time, updatedAt.Time
			list = append(list, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("secrets: list: %w", err)
	}
	return out, nil
}

// Delete removes a secret. Deleting one nobody has is ErrNotFound rather than
// a silent success, so a typo in an operator's command is visible.
func Delete(ctx context.Context, db *storage.DB, tenant tenancy.ID, name, actor string) error {
	if tenant == "" || actor == "" {
		return errors.New("secrets: tenant and actor are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(
			`DELETE FROM secrets WHERE tenant_id = ? AND name = ?`), string(tenant), name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return recordAudit(ctx, tx, db, tenant, name, "delete", actor, nil)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("secrets: delete %q: %w", name, err)
	}
	return nil
}

// valueAAD and dekAAD bind each seal to where it lives. The value's ciphertext
// is bound to its tenant and name; the wrapped data key additionally to the
// KEK that wrapped it. A row lifted into another tenant by a restore, a bad
// query or a hand-edited backup therefore fails to open — defence in depth
// under INV-T1, whose primary enforcement stays RLS.
func valueAAD(tenant tenancy.ID, name string) []byte {
	return []byte("lasterp/secret/value\x00" + string(tenant) + "\x00" + name)
}

func dekAAD(tenant tenancy.ID, name string) []byte {
	return []byte("lasterp/secret/dek\x00" + string(tenant) + "\x00" + name)
}

// recordAudit writes one attributable audit_log row (INV-T4), the same shape
// kernel/identity and kernel/capability use. changes never carries a value —
// only the name, and for a read, who read it.
func recordAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, name, action, actor string, extra map[string]any) error {
	fields := map[string]any{"name": name}
	for k, v := range extra {
		fields[k] = v
	}
	changes, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), "secret", name, action, string(changes), actor, time.Now().UTC()); err != nil {
		return fmt.Errorf("secrets: audit %s: %w", action, err)
	}
	return nil
}

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("secrets: stored value is not base64: %w", err)
	}
	return b, nil
}

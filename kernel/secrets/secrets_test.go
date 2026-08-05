// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.0: the secrets vault. Invariants: **INV-K1** (secret material is never
// persisted, logged, emitted or replicated in plaintext), INV-T1 (a tenant
// cannot reach another's secret), INV-T2/INV-T4 (reads and writes are
// authorized by a named principal and audited).

const (
	knownPlaintext = "hunter2-correct-horse-battery-staple"
	knownName      = "acme_api_key"
)

// --- helpers ---

// testSource builds an in-memory FileKeySource with the given key ids, the
// first being current. Constructed directly rather than through a file so the
// rotation tests can swap `current` without rewriting one.
func testSource(t *testing.T, ids ...string) *FileKeySource {
	t.Helper()
	src := &FileKeySource{current: ids[0], keys: map[string][]byte{}}
	for _, id := range ids {
		key, err := randomKey()
		if err != nil {
			t.Fatalf("randomKey: %v", err)
		}
		src.keys[id] = key
	}
	return src
}

func newTenant(t *testing.T, db *storage.DB) tenancy.ID {
	t.Helper()
	id := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, id, "vault test"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	return id
}

type rawRow struct{ keyID, wrappedDEK, ciphertext string }

func rawSecret(t *testing.T, db *storage.DB, tenant tenancy.ID, name string) rawRow {
	t.Helper()
	var r rawRow
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT key_id, wrapped_dek, ciphertext FROM secrets WHERE tenant_id = ? AND name = ?`),
			string(tenant), name).Scan(&r.keyID, &r.wrappedDEK, &r.ciphertext)
	})
	if err != nil {
		t.Fatalf("read raw secret row: %v", err)
	}
	return r
}

// auditRows returns the audit_log entries for a secret, newest last.
func auditRows(t *testing.T, db *storage.DB, tenant tenancy.ID, name string) []map[string]string {
	t.Helper()
	var out []map[string]string
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT action, actor_id, changes FROM audit_log
			 WHERE tenant_id = ? AND object = 'secret' AND record_id = ? ORDER BY at, id`),
			string(tenant), name)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var action, actor, changes string
			if err := rows.Scan(&action, &actor, &changes); err != nil {
				return err
			}
			out = append(out, map[string]string{"action": action, "actor": actor, "changes": changes})
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	return out
}

// assertNoPlaintext fails if s contains the known plaintext in raw or base64
// form. Base64 matters because the columns are base64 (decisions §10) — a
// vault that stored the value unencrypted would show up encoded, not raw, and
// a test looking only for the raw string would pass.
func assertNoPlaintext(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, knownPlaintext) {
		t.Errorf("INV-K1: %s contains the secret in plaintext: %s", where, s)
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte(knownPlaintext)); strings.Contains(s, b64) {
		t.Errorf("INV-K1: %s contains the base64 of the secret: %s", where, s)
	}
}

// --- INV-K1: nothing is stored in the clear ---

func TestSecretRoundTripsAndStoresNoPlaintext(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := testSource(t, "k1")
			tenant := newTenant(t, db)

			if err := Put(ctx, db, src, tenant, knownName, "acme", []byte(knownPlaintext), "user-1"); err != nil {
				t.Fatalf("Put: %v", err)
			}

			row := rawSecret(t, db, tenant, knownName)
			assertNoPlaintext(t, "the ciphertext column", row.ciphertext)
			assertNoPlaintext(t, "the wrapped_dek column", row.wrappedDEK)
			if row.keyID != "k1" {
				t.Errorf("key_id = %q, want k1", row.keyID)
			}

			got, err := Get(ctx, db, src, tenant, knownName, Reader{"module", "test"}, AllowAll)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(got) != knownPlaintext {
				t.Errorf("Get returned %q, want the stored value", string(got))
			}

			// Metadata is readable without touching key material at all.
			list, err := List(ctx, db, tenant)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 || list[0].Name != knownName || list[0].Description != "acme" {
				t.Fatalf("List = %+v, want one row for %s", list, knownName)
			}
			blob, err := json.Marshal(list)
			if err != nil {
				t.Fatalf("marshal list: %v", err)
			}
			assertNoPlaintext(t, "the List metadata", string(blob))
		})
	}
}

// A second write of the same value must not produce the same ciphertext: a
// fresh data key and nonce each time is what stops an observer of the column
// learning that two tenants (or two versions) share a value.
func TestRewritingASecretChangesItsCiphertext(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	src := testSource(t, "k1")
	tenant := newTenant(t, db)

	if err := Put(ctx, db, src, tenant, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first := rawSecret(t, db, tenant, knownName)
	if err := Put(ctx, db, src, tenant, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
		t.Fatalf("Put (again): %v", err)
	}
	second := rawSecret(t, db, tenant, knownName)

	if first.ciphertext == second.ciphertext {
		t.Error("INV-K1: storing the same value twice produced identical ciphertext")
	}
	if first.wrappedDEK == second.wrappedDEK {
		t.Error("re-writing a secret reused its data key; it should mint a fresh one")
	}
}

// Value is the type secret plaintext travels in, and the ordinary routes into
// a log line or a response body must not carry it.
func TestValueNeverRendersItsPlaintext(t *testing.T) {
	v := Value(knownPlaintext)

	assertNoPlaintext(t, "fmt %v", fmt.Sprintf("%v", v))
	assertNoPlaintext(t, "fmt %s", fmt.Sprintf("token=%s", v))
	assertNoPlaintext(t, "fmt %+v of a struct", fmt.Sprintf("%+v", struct{ Token Value }{v}))

	blob, err := json.Marshal(struct {
		Token Value `json:"token"`
	}{v})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertNoPlaintext(t, "encoding/json", string(blob))

	// The bytes are still reachable — deliberately, by an explicit conversion a
	// reviewer can see. If this ever stops holding the vault is useless.
	if string([]byte(v)) != knownPlaintext {
		t.Error("Value no longer yields its bytes on explicit conversion")
	}
}

// --- INV-T1: tenant isolation, and the AAD backstop under it ---

func TestSecretIsNotReadableByAnotherTenant(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := testSource(t, "k1")
			owner, other := newTenant(t, db), newTenant(t, db)

			if err := Put(ctx, db, src, owner, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if _, err := Get(ctx, db, src, other, knownName, Reader{"module", "test"}, AllowAll); !errors.Is(err, ErrNotFound) {
				t.Errorf("INV-T1: Get as another tenant = %v, want ErrNotFound", err)
			}
			list, err := List(ctx, db, other)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 0 {
				t.Errorf("INV-T1: another tenant's List returned %d rows, want 0", len(list))
			}
			if err := Delete(ctx, db, other, knownName, "user-2"); !errors.Is(err, ErrNotFound) {
				t.Errorf("INV-T1: Delete as another tenant = %v, want ErrNotFound", err)
			}
			// The owner's secret survived the other tenant's delete attempt.
			if _, err := Get(ctx, db, src, owner, knownName, Reader{"module", "test"}, AllowAll); err != nil {
				t.Errorf("owner's secret after a cross-tenant delete: %v", err)
			}
		})
	}
}

// The row's ciphertext is bound to its tenant and name as AAD, so a value
// copied into another row does not open. This is defence in depth *under*
// RLS — it is what stands up if a restore, a bad query or a hand-edited backup
// puts a row where it does not belong.
func TestCiphertextCopiedIntoAnotherRowDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	src := testSource(t, "k1")
	owner, thief := newTenant(t, db), newTenant(t, db)

	if err := Put(ctx, db, src, owner, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	row := rawSecret(t, db, owner, knownName)

	// Forge the copy the way a restore or a bad migration would: same bytes,
	// different tenant.
	err := tenancy.WithTenant(ctx, db, thief, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO secrets (tenant_id, name, description, key_id, wrapped_dek, ciphertext,
			                     created_at, updated_at, updated_by)
			VALUES (?, ?, '', ?, ?, ?, ?, ?, 'forged')`),
			string(thief), knownName, row.keyID, row.wrappedDEK, row.ciphertext,
			nowUTC(), nowUTC())
		return err
	})
	if err != nil {
		t.Fatalf("forge row: %v", err)
	}

	if _, err := Get(ctx, db, src, thief, knownName, Reader{"module", "test"}, AllowAll); err == nil {
		t.Fatal("INV-T1/INV-K1: a ciphertext copied into another tenant's row decrypted")
	}

	// Same key, same tenant, different name: also refused.
	err = tenancy.WithTenant(ctx, db, owner, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO secrets (tenant_id, name, description, key_id, wrapped_dek, ciphertext,
			                     created_at, updated_at, updated_by)
			VALUES (?, 'other_name', '', ?, ?, ?, ?, ?, 'forged')`),
			string(owner), row.keyID, row.wrappedDEK, row.ciphertext, nowUTC(), nowUTC())
		return err
	})
	if err != nil {
		t.Fatalf("forge row (same tenant): %v", err)
	}
	if _, err := Get(ctx, db, src, owner, "other_name", Reader{"module", "test"}, AllowAll); err == nil {
		t.Fatal("INV-K1: a ciphertext copied onto another name decrypted")
	}
}

// --- INV-T2/T4: a read names its reader, and every operation is recorded ---

func TestGetRequiresANamedAndGrantedReader(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	src := testSource(t, "k1")
	tenant := newTenant(t, db)
	if err := Put(ctx, db, src, tenant, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := Get(ctx, db, src, tenant, knownName, Reader{}, AllowAll); err == nil {
		t.Error("INV-T4: an anonymous reader was allowed to read a secret")
	}
	if _, err := Get(ctx, db, src, tenant, knownName, Reader{Kind: "plugin"}, AllowAll); err == nil {
		t.Error("INV-T4: a half-named reader was allowed to read a secret")
	}

	denyAll := func(Reader, string) bool { return false }
	if _, err := Get(ctx, db, src, tenant, knownName, Reader{"plugin", "com.acme.x"}, denyAll); !errors.Is(err, ErrForbidden) {
		t.Errorf("ungranted reader = %v, want ErrForbidden", err)
	}
	if _, err := Get(ctx, db, src, tenant, knownName, Reader{"plugin", "com.acme.x"}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("nil grants = %v, want ErrForbidden — a missing check must fail closed", err)
	}

	// A refused read is not a read: nothing was disclosed, so nothing is
	// recorded as disclosed.
	for _, row := range auditRows(t, db, tenant, knownName) {
		if row["action"] == "read" {
			t.Error("a refused read was recorded as a read")
		}
	}

	// The grants function sees what it is deciding about.
	var sawReader Reader
	var sawName string
	spy := func(r Reader, n string) bool { sawReader, sawName = r, n; return true }
	if _, err := Get(ctx, db, src, tenant, knownName, Reader{"plugin", "com.acme.x"}, spy); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sawReader.ID != "com.acme.x" || sawName != knownName {
		t.Errorf("grants saw (%v, %q), want the reader and the secret name", sawReader, sawName)
	}
}

func TestEveryVaultOperationIsAudited(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := testSource(t, "k1")
			tenant := newTenant(t, db)

			if err := Put(ctx, db, src, tenant, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if err := Put(ctx, db, src, tenant, knownName, "", []byte("replaced-value"), "user-2"); err != nil {
				t.Fatalf("Put (replace): %v", err)
			}
			if _, err := Get(ctx, db, src, tenant, knownName, Reader{"plugin", "com.acme.x"}, AllowAll); err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := Delete(ctx, db, tenant, knownName, "user-3"); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			rows := auditRows(t, db, tenant, knownName)
			var actions []string
			for _, r := range rows {
				actions = append(actions, r["action"])
				if r["actor"] == "" {
					t.Errorf("INV-T4: audit row %v has no actor", r)
				}
				assertNoPlaintext(t, "an audit row", r["changes"])
			}
			want := []string{"create", "update", "read", "delete"}
			if strings.Join(actions, ",") != strings.Join(want, ",") {
				t.Errorf("INV-T4: audit actions = %v, want %v", actions, want)
			}
			if rows[0]["actor"] != "user-1" || rows[1]["actor"] != "user-2" || rows[3]["actor"] != "user-3" {
				t.Errorf("INV-T4: writes are not attributed to their actor: %v", rows)
			}
			if !strings.Contains(rows[2]["changes"], "com.acme.x") {
				t.Errorf("a read must record who read it, got %q", rows[2]["changes"])
			}
		})
	}
}

func TestWritesRequireAnActorAndAValidName(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	src := testSource(t, "k1")
	tenant := newTenant(t, db)

	if err := Put(ctx, db, src, tenant, knownName, "", []byte(knownPlaintext), ""); err == nil {
		t.Error("INV-T4: an unattributed write was accepted")
	}
	if err := Delete(ctx, db, tenant, knownName, ""); err == nil {
		t.Error("INV-T4: an unattributed delete was accepted")
	}
	for _, bad := range []string{"", "Upper", "has space", "../etc/passwd", strings.Repeat("a", 129)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
		if err := Put(ctx, db, src, tenant, bad, "", []byte("v"), "user-1"); err == nil {
			t.Errorf("Put accepted the invalid name %q", bad)
		}
	}
	if err := Put(ctx, db, src, tenant, knownName, "", nil, "user-1"); err == nil {
		t.Error("Put accepted an empty value")
	}
}

func TestVaultWritesRefuseWithoutAKeySource(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)

	if err := Put(ctx, db, nil, tenant, knownName, "", []byte(knownPlaintext), "user-1"); !errors.Is(err, ErrNoKeySource) {
		t.Errorf("Put without a key source = %v, want ErrNoKeySource", err)
	}
	if _, err := Get(ctx, db, nil, tenant, knownName, Reader{"module", "test"}, AllowAll); !errors.Is(err, ErrNoKeySource) {
		t.Errorf("Get without a key source = %v, want ErrNoKeySource", err)
	}
	if _, err := Rotate(ctx, db, nil, tenant, "operator"); !errors.Is(err, ErrNoKeySource) {
		t.Errorf("Rotate without a key source = %v, want ErrNoKeySource", err)
	}
}

// --- AC: rotation re-wraps without re-encrypting the payloads ---

func TestRotationRewrapsWithoutTouchingCiphertext(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			old := testSource(t, "k-old")
			tenant := newTenant(t, db)

			for _, n := range []string{knownName, "other_key"} {
				if err := Put(ctx, db, old, tenant, n, "", []byte(knownPlaintext), "user-1"); err != nil {
					t.Fatalf("Put %s: %v", n, err)
				}
			}
			before := rawSecret(t, db, tenant, knownName)

			// The new source holds both keys: the old one must stay readable
			// while any row still references it.
			rotated := &FileKeySource{current: "k-new", keys: map[string][]byte{
				"k-old": old.keys["k-old"],
				"k-new": mustKey(t),
			}}

			n, err := Rotate(ctx, db, rotated, tenant, "operator")
			if err != nil {
				t.Fatalf("Rotate: %v", err)
			}
			if n != 2 {
				t.Errorf("Rotate moved %d secrets, want 2", n)
			}

			after := rawSecret(t, db, tenant, knownName)
			if after.ciphertext != before.ciphertext {
				t.Error("rotation re-encrypted the payload; it must only re-wrap the data key")
			}
			if after.wrappedDEK == before.wrappedDEK {
				t.Error("rotation left the wrapped data key unchanged")
			}
			if after.keyID != "k-new" {
				t.Errorf("key_id after rotation = %q, want k-new", after.keyID)
			}

			got, err := Get(ctx, db, rotated, tenant, knownName, Reader{"module", "test"}, AllowAll)
			if err != nil {
				t.Fatalf("Get after rotation: %v", err)
			}
			if string(got) != knownPlaintext {
				t.Error("the secret did not survive rotation")
			}

			// Rotation is idempotent: a second pass finds nothing to do, which
			// is what makes a crashed run safe to re-run.
			again, err := Rotate(ctx, db, rotated, tenant, "operator")
			if err != nil {
				t.Fatalf("Rotate (again): %v", err)
			}
			if again != 0 {
				t.Errorf("re-running rotation moved %d secrets, want 0", again)
			}

			// The re-wrap is a mutation, so it is attributable like any other.
			var rotates int
			for _, r := range auditRows(t, db, tenant, knownName) {
				if r["action"] == "rotate" {
					rotates++
					if r["actor"] != "operator" {
						t.Errorf("INV-T4: rotate audited to %q", r["actor"])
					}
					assertNoPlaintext(t, "a rotation audit row", r["changes"])
				}
			}
			if rotates != 1 {
				t.Errorf("rotation wrote %d audit rows, want 1", rotates)
			}
		})
	}
}

// Retiring a key before rotation has drained it is the one operator mistake
// that destroys data, so it fails loudly and by name, and leaves the row where
// it was.
func TestRotationRefusesWhenTheOldKeyIsGone(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	old := testSource(t, "k-old")
	tenant := newTenant(t, db)
	if err := Put(ctx, db, old, tenant, knownName, "", []byte(knownPlaintext), "user-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before := rawSecret(t, db, tenant, knownName)

	orphan := testSource(t, "k-new") // the old key is not in it
	n, err := Rotate(ctx, db, orphan, tenant, "operator")
	if err == nil {
		t.Fatal("rotation succeeded without the key that sealed the row")
	}
	if !strings.Contains(err.Error(), "k-old") {
		t.Errorf("error = %v; it must name the missing key", err)
	}
	if n != 0 {
		t.Errorf("rotation reported %d moved before failing, want 0", n)
	}
	if after := rawSecret(t, db, tenant, knownName); after != before {
		t.Error("a failed rotation modified the row")
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	k, err := randomKey()
	if err != nil {
		t.Fatalf("randomKey: %v", err)
	}
	return k
}

func nowUTC() time.Time { return time.Now().UTC() }

// --- sanity on the primitive itself ---

func TestSealBindsItsAAD(t *testing.T) {
	key := mustKey(t)
	sealed, err := seal(key, []byte(knownPlaintext), []byte("aad-a"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(knownPlaintext)) {
		t.Error("INV-K1: the sealed bytes contain the plaintext")
	}
	if _, err := open(key, sealed, []byte("aad-b")); err == nil {
		t.Error("a value opened under the wrong AAD")
	}
	if _, err := open(mustKey(t), sealed, []byte("aad-a")); err == nil {
		t.Error("a value opened under the wrong key")
	}
	got, err := open(key, sealed, []byte("aad-a"))
	if err != nil || string(got) != knownPlaintext {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Flip one byte of the ciphertext: GCM must refuse it rather than return
	// mangled plaintext.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := open(key, tampered, []byte("aad-a")); err == nil {
		t.Error("a tampered ciphertext opened")
	}
}

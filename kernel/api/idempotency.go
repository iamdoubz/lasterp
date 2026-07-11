// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// idempotencyStore persists write responses keyed by (tenant, Idempotency-
// Key) so a replay returns the identical result instead of re-executing
// (ADR-009). Reserve-first: a pending row (response_status = 0) is inserted
// before the write runs, so a concurrent duplicate observes the reservation
// rather than double-executing.
type idempotencyStore struct {
	db *storage.DB
}

const statusPending = 0

// fingerprint is the SHA-256 of method+path+body, so reusing a key for a
// different request is detectable (ErrKeyConflict) rather than silently
// replaying an unrelated response.
func fingerprint(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// storedResponse is a finalized idempotent response.
type storedResponse struct {
	status int
	body   []byte
}

var (
	// errKeyConflict: the key was reused with a different request, or is
	// still in flight — either way the caller must not proceed.
	errKeyConflict = errors.New("api: idempotency key conflict")
	// errReserved is the sentinel begin returns when it freshly reserved the
	// key and the caller should execute the write.
	errReserved = errors.New("api: idempotency key reserved")
)

// begin either reserves fp under (tenant, key) and returns errReserved (the
// caller executes the write), or, when the key already exists, returns the
// stored response to replay (nil error) or errKeyConflict for a fingerprint
// mismatch / still-pending reservation.
func (s *idempotencyStore) begin(ctx context.Context, tenant tenancy.ID, key, fp string) (*storedResponse, error) {
	if existing, err := s.lookup(ctx, tenant, key); err != nil {
		return nil, err
	} else if existing != nil {
		return decideExisting(existing, fp)
	}

	err := tenancy.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`
			INSERT INTO idempotency_keys (tenant_id, idem_key, request_fingerprint, response_status, response_body, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`),
			string(tenant), key, fp, statusPending, "", time.Now().UTC())
		return err
	})
	if err == nil {
		return nil, errReserved
	}
	if storage.IsUniqueViolation(err) {
		// Lost a race to reserve: re-read and decide against the winner.
		existing, lerr := s.lookup(ctx, tenant, key)
		if lerr != nil {
			return nil, lerr
		}
		if existing == nil {
			return nil, errKeyConflict
		}
		return decideExisting(existing, fp)
	}
	return nil, fmt.Errorf("api: reserve idempotency key: %w", err)
}

type storedRow struct {
	fingerprint string
	status      int
	body        string
}

func decideExisting(row *storedRow, fp string) (*storedResponse, error) {
	if row.fingerprint != fp {
		return nil, errKeyConflict
	}
	if row.status == statusPending {
		return nil, errKeyConflict
	}
	return &storedResponse{status: row.status, body: []byte(row.body)}, nil
}

func (s *idempotencyStore) lookup(ctx context.Context, tenant tenancy.ID, key string) (*storedRow, error) {
	var row *storedRow
	err := tenancy.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var r storedRow
		scanErr := tx.QueryRowContext(ctx, s.db.Rebind(`
			SELECT request_fingerprint, response_status, response_body
			FROM idempotency_keys WHERE tenant_id = ? AND idem_key = ?`),
			string(tenant), key).Scan(&r.fingerprint, &r.status, &r.body)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		row = &r
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("api: lookup idempotency key: %w", err)
	}
	return row, nil
}

// finalize records the executed write's response under the reserved key.
func (s *idempotencyStore) finalize(ctx context.Context, tenant tenancy.ID, key string, status int, body []byte) error {
	err := tenancy.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE idempotency_keys SET response_status = ?, response_body = ?
			WHERE tenant_id = ? AND idem_key = ?`),
			status, string(body), string(tenant), key)
		return err
	})
	if err != nil {
		return fmt.Errorf("api: finalize idempotency key: %w", err)
	}
	return nil
}

// discard removes a reservation whose write did not succeed, so the client
// may legitimately retry the same key.
func (s *idempotencyStore) discard(ctx context.Context, tenant tenancy.ID, key string) error {
	err := tenancy.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`
			DELETE FROM idempotency_keys WHERE tenant_id = ? AND idem_key = ?`),
			string(tenant), key)
		return err
	})
	if err != nil {
		return fmt.Errorf("api: discard idempotency key: %w", err)
	}
	return nil
}

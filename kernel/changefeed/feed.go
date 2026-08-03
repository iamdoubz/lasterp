// SPDX-License-Identifier: AGPL-3.0-only

// Package changefeed is the totally-ordered, per-tenant log a replica follows
// to stay in step with the server (docs/04 §Downstream, ADR-004).
//
// The package is named changefeed rather than sync so that files here can use
// the standard library's sync without an import alias.
//
// Entries are pointers: each names a row in events or audit_log rather than
// carrying its payload, so financial truth stays in exactly one place
// (WP-2.1-decisions.md §3). Callers append inside the same transaction as the
// write being described; a rolled-back write leaves no feed entry, because
// there is no second commit to lose.
package changefeed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Source names the table an entry points at.
type Source string

const (
	// SourceEvent points at events.id — the event store (ADR-003).
	SourceEvent Source = "event"
	// SourceAudit points at audit_log.id — metadata CRUD mutations.
	SourceAudit Source = "audit"
)

// Entry is one change to publish, as supplied by the writer.
// Field names are wire-visible: GET /api/v1/sync/changes serialises these
// straight out, so they carry snake_case tags like every other payload in the
// API rather than exposing Go's exported-field casing to clients.
type Entry struct {
	TenantID tenancy.ID `json:"tenant_id"`
	Source   Source     `json:"source"`
	// RefID is the primary key of the thing that changed — the event for
	// SourceEvent, the *record* for SourceAudit (not the audit row: see
	// kernel/metadata's recordAudit).
	RefID      string    `json:"ref_id"`
	Object     string    `json:"object"` // object/stream kind, for scope tagging and filtering
	ScopeKey   string    `json:"scope_key"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Change is an Entry as read back, carrying the cursor position the reader
// resumes from.
type Change struct {
	Cursor int64 `json:"cursor"`
	Entry
}

// Append writes one feed entry inside the caller's transaction. tx must
// already carry tenant context (tenancy.SetContext), exactly as the write it
// describes does — the entry is subject to the same RLS policy as the row it
// points at.
func Append(ctx context.Context, tx *sql.Tx, db *storage.DB, e Entry) error {
	if e.TenantID == "" {
		return errors.New("changefeed: tenant is required")
	}
	if e.Source == "" || e.RefID == "" {
		return errors.New("changefeed: source and ref id are required")
	}
	if e.RecordedAt.IsZero() {
		return errors.New("changefeed: recorded at is required")
	}

	// Take the per-tenant ordering lock before allocating a cursor position,
	// so this entry's id and its commit land in the same relative order as
	// every other writer's (INV-S5 — see order.go).
	if err := serializeAppend(ctx, tx, db, e.TenantID); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO change_feed (tenant_id, source, ref_id, object, scope_key, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		string(e.TenantID), string(e.Source), e.RefID, e.Object, e.ScopeKey, e.RecordedAt.UTC())
	if err != nil {
		return fmt.Errorf("changefeed: append: %w", err)
	}
	return nil
}

// Read returns up to limit changes for tenant after the given cursor, in
// ascending cursor order.
//
// Callers resume by passing back the last Change's Cursor. A reader that has
// consumed everything currently visible gets an empty slice, not an error.
func Read(ctx context.Context, db *storage.DB, tenant tenancy.ID, after int64, limit int) ([]Change, error) {
	if tenant == "" {
		return nil, errors.New("changefeed: tenant is required")
	}
	if limit <= 0 {
		return nil, errors.New("changefeed: limit must be positive")
	}

	var changes []Change
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, tenant_id, source, ref_id, object, scope_key, recorded_at
			FROM change_feed
			WHERE tenant_id = ? AND id > ?
			ORDER BY id ASC LIMIT ?`),
			string(tenant), after, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			c, err := scanChange(rows)
			if err != nil {
				return err
			}
			changes = append(changes, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("changefeed: read: %w", err)
	}
	return changes, nil
}

// HighWater returns the newest cursor position visible for tenant, or 0 when
// the feed is empty.
//
// It is what a hydrating client records before paging a snapshot: everything
// that changes from here on is in the feed, so the snapshot's own raciness is
// repaired by the first batch rather than prevented by holding a transaction
// open across the whole hydration (WP-2.2-decisions.md §4).
func HighWater(ctx context.Context, db *storage.DB, tenant tenancy.ID) (int64, error) {
	if tenant == "" {
		return 0, errors.New("changefeed: tenant is required")
	}

	var cursor int64
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// COALESCE rather than scanning a NULL: an empty feed is position 0,
		// which is exactly the cursor a client starts from.
		row := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT COALESCE(MAX(id), 0) FROM change_feed WHERE tenant_id = ?`), string(tenant))
		return row.Scan(&cursor)
	})
	if err != nil {
		return 0, fmt.Errorf("changefeed: high water: %w", err)
	}
	return cursor, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanChange(row scanner) (Change, error) {
	var c Change
	var tenantStr, sourceStr string
	var recordedAt storage.Time
	err := row.Scan(&c.Cursor, &tenantStr, &sourceStr, &c.RefID, &c.Object, &c.ScopeKey, &recordedAt)
	if err != nil {
		return Change{}, fmt.Errorf("changefeed: scan change: %w", err)
	}
	c.TenantID = tenancy.ID(tenantStr)
	c.Source = Source(sourceStr)
	c.RecordedAt = recordedAt.Time
	return c, nil
}

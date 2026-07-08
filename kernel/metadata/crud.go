// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Record is a generic, schema-shaped row: keys are field names (plus
// id/tenant_id/created_at/updated_at/archived_at), values are Go-typed
// per the field's FieldType. Core fields are real columns; overlay fields
// are transparently stored in/read from the generated table's
// custom_fields blob (see GenerateDDL) — callers don't need to know which
// is which.
type Record map[string]any

// CRUD is a generic runtime engine for one CRUD-persistence object,
// driven entirely by its EffectiveSchema — this is what WP-0.5's
// "codegen" produces (decision 1: metadata-driven runtime behavior, not
// emitted Go source). Every method requires an authz.Actor via context
// (INV-T2/INV-T4, checked through kernel/authz.Authorize using the
// schema's declared permissions) and runs through tenancy.WithTenant
// (INV-T1).
type CRUD struct {
	schema *EffectiveSchema
}

// ErrWrongPersistence is returned by NewCRUD for a non-CRUD object
// (decision 2: event-sourced codegen is out of scope for this WP).
var ErrWrongPersistence = errors.New("metadata: CRUD engine requires persistence \"crud\"")

// ErrValidation covers a Record failing the schema's required-field check.
var ErrValidation = errors.New("metadata: validation failed")

// ErrRecordNotFound is returned by Get/Update/SoftDelete for an unknown
// (or another tenant's) id.
var ErrRecordNotFound = errors.New("metadata: record not found")

// NewCRUD builds a CRUD engine for schema.
func NewCRUD(schema *EffectiveSchema) (*CRUD, error) {
	if schema.Persistence != PersistenceCRUD {
		return nil, fmt.Errorf("%w (got %q)", ErrWrongPersistence, schema.Persistence)
	}
	return &CRUD{schema: schema}, nil
}

func (c *CRUD) validate(rec Record) error {
	for _, f := range c.schema.Fields {
		if !f.Required {
			continue
		}
		v, present := rec[f.Name]
		if !present || v == nil || v == "" {
			return fmt.Errorf("%w: field %q is required", ErrValidation, f.Name)
		}
	}
	return nil
}

// Create inserts rec, requiring the "create" permission (authz.Authorize)
// and recording an audit_log entry in the same transaction as the insert.
func (c *CRUD) Create(ctx context.Context, db *storage.DB, tenant tenancy.ID, rec Record) (Record, error) {
	actor, err := authz.Authorize(ctx, db, c.schema.ObjectName, "create")
	if err != nil {
		return nil, err
	}
	if err := c.validate(rec); err != nil {
		return nil, err
	}

	id := idgen.New()
	now := time.Now().UTC()
	table := TableName(c.schema.ObjectName)

	var result Record
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		cols := []string{"id", "tenant_id"}
		vals := []any{id, string(tenant)}
		customFields := map[string]any{}
		for _, f := range c.schema.Fields {
			if f.FromOverlay {
				if v, ok := rec[f.Name]; ok {
					customFields[f.Name] = v
				}
				continue
			}
			cols = append(cols, f.Name)
			vals = append(vals, rec[f.Name])
		}
		customJSON, err := json.Marshal(customFields)
		if err != nil {
			return err
		}
		cols = append(cols, "custom_fields", "created_at", "updated_at")
		vals = append(vals, string(customJSON), now, now)

		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), placeholders)
		if _, err := tx.ExecContext(ctx, db.Rebind(query), vals...); err != nil {
			return err
		}

		changes, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, db, tenant, c.schema.ObjectName, id, "create", changes, string(actor.UserID)); err != nil {
			return err
		}

		result = cloneRecord(rec)
		result["id"] = id
		result["tenant_id"] = string(tenant)
		result["created_at"] = now
		result["updated_at"] = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Get returns one record by id, requiring the "read" permission.
func (c *CRUD) Get(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (Record, error) {
	if _, err := authz.Authorize(ctx, db, c.schema.ObjectName, "read"); err != nil {
		return nil, err
	}

	var rec Record
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		got, err := c.getTx(ctx, tx, db, tenant, id)
		if err != nil {
			return err
		}
		if got == nil {
			return ErrRecordNotFound
		}
		rec = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (c *CRUD) getTx(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, id string) (Record, error) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE tenant_id = ? AND id = ?", selectColumns(c.schema), TableName(c.schema.ObjectName))
	row := tx.QueryRowContext(ctx, db.Rebind(query), string(tenant), id)
	rec, err := scanRecord(row, c.schema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// List returns every non-archived record for tenant, requiring the
// "read" permission.
func (c *CRUD) List(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Record, error) {
	if _, err := authz.Authorize(ctx, db, c.schema.ObjectName, "read"); err != nil {
		return nil, err
	}

	var records []Record
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		query := fmt.Sprintf("SELECT %s FROM %s WHERE tenant_id = ? AND archived_at IS NULL ORDER BY created_at ASC",
			selectColumns(c.schema), TableName(c.schema.ObjectName))
		rows, err := tx.QueryContext(ctx, db.Rebind(query), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			rec, err := scanRecord(rows, c.schema)
			if err != nil {
				return err
			}
			records = append(records, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// Update applies changes (a partial Record — only present keys are
// touched) to id, requiring the "update" permission. The audit entry
// records old and new values for each changed field (ADR-003: CRUD audit
// captures "old/new values, actor, timestamp"). A touched overlay field
// is merged into the custom_fields blob (read-modify-write, seeded from
// every overlay field's current value so untouched ones survive the
// re-marshal).
func (c *CRUD) Update(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string, changes Record) (Record, error) {
	actor, err := authz.Authorize(ctx, db, c.schema.ObjectName, "update")
	if err != nil {
		return nil, err
	}

	table := TableName(c.schema.ObjectName)
	var result Record
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		current, err := c.getTx(ctx, tx, db, tenant, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrRecordNotFound
		}

		diff := make(map[string]map[string]any, len(changes))
		var setClauses []string
		var vals []any
		customFields := map[string]any{}
		customTouched := false
		for _, f := range c.schema.Fields {
			if f.FromOverlay {
				if v, ok := current[f.Name]; ok {
					customFields[f.Name] = v
				}
			}
		}
		for _, f := range c.schema.Fields {
			newVal, touched := changes[f.Name]
			if !touched {
				continue
			}
			diff[f.Name] = map[string]any{"old": current[f.Name], "new": newVal}
			if f.FromOverlay {
				customFields[f.Name] = newVal
				customTouched = true
			} else {
				setClauses = append(setClauses, f.Name+" = ?")
				vals = append(vals, newVal)
			}
		}
		if customTouched {
			customJSON, err := json.Marshal(customFields)
			if err != nil {
				return err
			}
			setClauses = append(setClauses, "custom_fields = ?")
			vals = append(vals, string(customJSON))
		}
		now := time.Now().UTC()
		setClauses = append(setClauses, "updated_at = ?")
		vals = append(vals, now, string(tenant), id)

		query := fmt.Sprintf("UPDATE %s SET %s WHERE tenant_id = ? AND id = ?", table, strings.Join(setClauses, ", "))
		if _, err := tx.ExecContext(ctx, db.Rebind(query), vals...); err != nil {
			return err
		}

		changesJSON, err := json.Marshal(diff)
		if err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, db, tenant, c.schema.ObjectName, id, "update", changesJSON, string(actor.UserID)); err != nil {
			return err
		}

		merged := cloneRecord(current)
		for name, v := range changes {
			merged[name] = v
		}
		merged["updated_at"] = now
		result = merged
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SoftDelete sets archived_at (docs/03: "Soft-delete only for CRUD
// objects"), requiring the "delete" permission.
func (c *CRUD) SoftDelete(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	actor, err := authz.Authorize(ctx, db, c.schema.ObjectName, "delete")
	if err != nil {
		return err
	}

	table := TableName(c.schema.ObjectName)
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		current, err := c.getTx(ctx, tx, db, tenant, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrRecordNotFound
		}

		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, db.Rebind(fmt.Sprintf(`UPDATE %s SET archived_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`, table)),
			now, now, string(tenant), id)
		if err != nil {
			return err
		}

		changes, err := json.Marshal(map[string]any{"archived_at": map[string]any{"old": nil, "new": now}})
		if err != nil {
			return err
		}
		return recordAudit(ctx, tx, db, tenant, c.schema.ObjectName, id, "delete", changes, string(actor.UserID))
	})
}

func cloneRecord(rec Record) Record {
	out := make(Record, len(rec))
	for k, v := range rec {
		out[k] = v
	}
	return out
}

// selectColumns lists only core (non-overlay) fields as named columns,
// plus the fixed custom_fields blob overlay fields are read from — see
// GenerateDDL and scanRecord.
func selectColumns(schema *EffectiveSchema) string {
	cols := make([]string, 0, len(schema.Fields)+6)
	cols = append(cols, "id", "tenant_id")
	for _, f := range schema.Fields {
		if f.FromOverlay {
			continue
		}
		cols = append(cols, f.Name)
	}
	cols = append(cols, "custom_fields", "created_at", "updated_at", "archived_at")
	return strings.Join(cols, ", ")
}

// rowScanner is the common subset of *sql.Row and *sql.Rows scanRecord
// needs, so a single-row lookup and a list query share one scan body
// (same pattern as kernel/eventstore's scanner).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner, schema *EffectiveSchema) (Record, error) {
	var id, tenantID, customFieldsJSON string
	dest := []any{&id, &tenantID}

	var coreFields []Field
	var fieldDest []any
	for _, f := range schema.Fields {
		if f.FromOverlay {
			continue
		}
		coreFields = append(coreFields, f)
		var d any
		switch f.Type {
		case FieldInt:
			d = new(sql.NullInt64)
		case FieldBool:
			d = new(sql.NullBool)
		case FieldDate, FieldDatetime:
			d = new(storage.NullTime)
		default:
			d = new(sql.NullString)
		}
		fieldDest = append(fieldDest, d)
		dest = append(dest, d)
	}

	var createdAt, updatedAt storage.Time
	var archivedAt storage.NullTime
	dest = append(dest, &customFieldsJSON, &createdAt, &updatedAt, &archivedAt)

	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	rec := Record{
		"id": id, "tenant_id": tenantID,
		"created_at": createdAt.Time, "updated_at": updatedAt.Time,
	}
	if archivedAt.Valid {
		rec["archived_at"] = archivedAt.Time
	}
	for i, f := range coreFields {
		rec[f.Name] = derefFieldValue(fieldDest[i])
	}

	var customFields map[string]any
	if err := json.Unmarshal([]byte(customFieldsJSON), &customFields); err != nil {
		return nil, fmt.Errorf("metadata: unmarshal custom_fields: %w", err)
	}
	for _, f := range schema.Fields {
		if !f.FromOverlay {
			continue
		}
		if v, ok := customFields[f.Name]; ok {
			rec[f.Name] = v
		}
	}
	return rec, nil
}

func derefFieldValue(dest any) any {
	switch v := dest.(type) {
	case *sql.NullInt64:
		if v.Valid {
			return v.Int64
		}
	case *sql.NullBool:
		if v.Valid {
			return v.Bool
		}
	case *storage.NullTime:
		if v.Valid {
			return v.Time
		}
	case *sql.NullString:
		if v.Valid {
			return v.String
		}
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// ErrUnsupportedFieldType is returned by GenerateDDL for a field type it
// cannot map to a single column — currently just FieldTable (a child-row
// relationship needs its own generated table + FK, out of scope for
// WP-0.5's create-only DDL generation — decision 3).
var ErrUnsupportedFieldType = errors.New("metadata: field type not supported by DDL generation")

// TableName returns the generated table name for an object.
func TableName(objectName string) string {
	return "obj_" + strings.ToLower(objectName)
}

func columnType(t FieldType) (string, error) {
	switch t {
	case FieldText, FieldLongText, FieldRichText, FieldEmail, FieldPhone,
		FieldAddress, FieldEnum, FieldLink, FieldJSON, FieldFile,
		FieldDuration, FieldComputed, FieldDecimal, FieldMoney, FieldPercent, FieldCurrency:
		// Money/decimal/percent are stored as TEXT (exact string, e.g. a
		// JSON {"amount":..,"currency":".."} for money) rather than a
		// floating-point column — CLAUDE.md: "Money: integer minor units
		// ... never float." A first-class two-column money representation
		// is WP-1.1's job; this is a portable placeholder until then.
		return "TEXT", nil
	case FieldInt:
		return "INT", nil
	case FieldBool:
		return "BOOLEAN", nil
	case FieldDate, FieldDatetime:
		return "TIMESTAMPTZ", nil
	case FieldTable:
		return "", fmt.Errorf("%w: %q (child-row relationships need their own generated table)", ErrUnsupportedFieldType, t)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedFieldType, t)
	}
}

// GenerateDDL produces the CREATE TABLE (+ indexes, + Postgres RLS) DDL
// for schema's first version. Diff-based ALTER planning for evolving an
// already-deployed object is out of scope (decision 3).
//
// Only core (non-overlay) fields become physical columns: two tenants can
// overlay the same core object differently, so the one shared table every
// tenant uses can't have a fixed column per overlay field. Overlay fields
// live in a fixed custom_fields TEXT column (a JSON blob), per ADR-006
// ("Custom fields for core objects store in a JSONB column with generated
// typed accessors"). Expression-indexing into custom_fields is a future
// capability, not v1 — an Index:true overlay field is simply not indexed.
func GenerateDDL(schema *EffectiveSchema, dialect storage.Dialect) (string, error) {
	table := TableName(schema.ObjectName)

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n\tid TEXT PRIMARY KEY,\n\ttenant_id TEXT NOT NULL,\n", table)
	for _, f := range schema.Fields {
		if f.FromOverlay {
			continue
		}
		colType, err := columnType(f.Type)
		if err != nil {
			return "", err
		}
		nullability := ""
		if f.Required {
			nullability = " NOT NULL"
		}
		fmt.Fprintf(&b, "\t%s %s%s,\n", f.Name, colType, nullability)
	}
	b.WriteString("\tcustom_fields TEXT NOT NULL DEFAULT '{}',\n")
	b.WriteString("\tcreated_at TIMESTAMPTZ NOT NULL,\n\tupdated_at TIMESTAMPTZ NOT NULL,\n\tarchived_at TIMESTAMPTZ\n);\n")

	fmt.Fprintf(&b, "CREATE INDEX idx_%s_tenant_id ON %s (tenant_id);\n", table, table)
	for _, f := range schema.Fields {
		if f.Index && !f.FromOverlay {
			fmt.Fprintf(&b, "CREATE INDEX idx_%s_%s ON %s (tenant_id, %s);\n", table, f.Name, table, f.Name)
		}
	}

	if dialect == storage.Postgres {
		fmt.Fprintf(&b, "ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", table)
		fmt.Fprintf(&b, "ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n", table)
		fmt.Fprintf(&b, "CREATE POLICY tenant_isolation_%s ON %s USING (tenant_id = current_setting('app.tenant_id', true));\n", table, table)
	}

	return b.String(), nil
}

// ApplyDDL generates and executes schema's DDL for version, tracked in
// object_schema_migrations so re-applying the same version is a no-op
// (idempotent) rather than erroring on "table already exists".
//
// This is a global operation, not a per-tenant one: the physical table it
// creates is shared across every tenant (that's the point of the
// tenant_id column + RLS policy GenerateDDL adds), so there is exactly
// one "has this object's DDL been applied yet" answer, not one per
// tenant — the bug this fixes: an earlier version scoped the tracking row
// by tenant, so a second tenant's first ApplyDDL call would try to
// CREATE TABLE a table the first tenant had already created.
func ApplyDDL(ctx context.Context, db *storage.DB, schema *EffectiveSchema, version int) error {
	applied, err := isDDLApplied(ctx, db, schema.ObjectName, version)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	ddl, err := GenerateDDL(schema, db.Dialect)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metadata: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("metadata: apply DDL: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO object_schema_migrations (object, version, applied_at)
		VALUES (?, ?, ?)`),
		schema.ObjectName, version, time.Now().UTC()); err != nil {
		return fmt.Errorf("metadata: record applied DDL: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metadata: commit: %w", err)
	}
	return nil
}

func isDDLApplied(ctx context.Context, db *storage.DB, object string, version int) (bool, error) {
	var n int
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT COUNT(*) FROM object_schema_migrations WHERE object = ? AND version = ?`),
		object, version)
	if err := row.Scan(&n); err != nil {
		return false, fmt.Errorf("metadata: check applied DDL: %w", err)
	}
	return n > 0, nil
}

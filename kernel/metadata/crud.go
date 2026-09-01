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
	// hooks is the optional synchronous plugin dispatch seam (WP-3.1b,
	// hooks.go). Nil — the default — is exactly today's behaviour.
	hooks Hooks
}

// ErrWrongPersistence is returned by NewCRUD for a non-CRUD object
// (decision 2: event-sourced codegen is out of scope for this WP).
var ErrWrongPersistence = errors.New("metadata: CRUD engine requires persistence \"crud\"")

// ErrValidation covers a Record failing the effective schema: a missing
// required field, a value of the wrong type for its FieldType, or an enum
// value outside the field's declared options (INV-T5).
var ErrValidation = errors.New("metadata: validation failed")

// ErrRecordNotFound is returned by Get/Update/SoftDelete for an unknown
// (or another tenant's) id.
var ErrRecordNotFound = errors.New("metadata: record not found")

// ErrIDTaken is returned by Create when the caller supplied an id that
// already exists. It maps to a 409 whose detail says only that the id is
// taken: the primary key is global per table, so a fuller answer would tell
// a caller whether some other tenant holds that row (INV-T1).
var ErrIDTaken = errors.New("metadata: id already exists")

// NewCRUD builds a CRUD engine for schema.
func NewCRUD(schema *EffectiveSchema) (*CRUD, error) {
	if schema.Persistence != PersistenceCRUD {
		return nil, fmt.Errorf("%w (got %q)", ErrWrongPersistence, schema.Persistence)
	}
	return &CRUD{schema: schema}, nil
}

// validated checks rec against the effective schema and returns a normalized
// copy — the values as they should be stored (INV-T5). It never mutates rec.
//
// partial is the Update path: only the keys actually present are checked, so a
// row that already holds an out-of-set value stays editable on every other
// field. That matters because validation landing in WP-1.11 must not strand
// rows written before it: the read path does not validate, and an update that
// does not touch the offending field is not the place to relitigate it. Setting
// the field to a legal value is the fix, and it is one request.
//
// A required field being *touched* is still held to required-ness on update —
// nulling one is refused — because that is a write of an invalid value, not the
// absence of a write.
func (c *CRUD) validated(rec Record, partial bool) (Record, error) {
	out := cloneRecord(rec)
	for _, f := range c.schema.Fields {
		v, present := rec[f.Name]
		if partial && !present {
			continue
		}
		if f.Required && (!present || v == nil || v == "") {
			return nil, fmt.Errorf("%w: field %q is required", ErrValidation, f.Name)
		}
		if !present {
			continue
		}
		normalized, err := validateValue(f, v)
		if err != nil {
			return nil, err
		}
		out[f.Name] = normalized
	}
	return out, nil
}

// recordID resolves the primary key for a new row: the caller's, when it
// supplied one, and a fresh UUIDv7 otherwise.
//
// Honouring a caller-supplied id is what lets an offline client apply a
// create optimistically and have the row it is looking at *be* the row the
// server ends up with (WP-2.3-decisions.md §2). The alternative — a
// provisional local id rewritten on acceptance, and every queued command
// that references it rewritten too — is the largest single piece of client
// logic the outbox could have contained, and it buys nothing: this is a
// UUIDv7 surrogate key, not a sequence. INV-F6's human-visible numbers
// (invoice numbers) are unaffected; they are still allocated server-side at
// acceptance.
//
// A malformed id is refused rather than quietly replaced. Inventing an id
// for a caller that sent a bad one means the row it believes it created is
// not the row that exists — the same silent divergence, arrived at from the
// other side.
func recordID(rec Record) (string, error) {
	raw, present := rec["id"]
	if !present || raw == nil || raw == "" {
		return idgen.New(), nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: field %q must be a UUIDv7 string, got %T", ErrValidation, "id", raw)
	}
	if !idgen.IsV7(s) {
		return "", fmt.Errorf("%w: field %q is not a canonical UUIDv7", ErrValidation, "id")
	}
	return s, nil
}

// Create inserts rec, requiring the "create" permission (authz.Authorize)
// and recording an audit_log entry in the same transaction as the insert.
func (c *CRUD) Create(ctx context.Context, db *storage.DB, tenant tenancy.ID, rec Record) (Record, error) {
	// Authorization first, validation second, always: a 422 that names the
	// legal values for a caller who is not allowed to write at all is a
	// validity oracle (INV-T2). The same ordering holds in Update.
	actor, grants, err := authz.AuthorizeGrants(ctx, db, c.schema.ObjectName, "create")
	if err != nil {
		return nil, err
	}
	rec, err = c.validated(rec, false)
	if err != nil {
		return nil, err
	}
	// Sync hooks run here: after the record is known valid, before any
	// transaction exists (hooks.go). A rejection is the plugin's, and reaches
	// the caller unchanged.
	rec, err = c.runBefore(ctx, tenant, VerbCreate, rec, false)
	if err != nil {
		return nil, err
	}
	// Second stage of the gate (WP-3.3a): a conditional grant is judged against
	// the record it is about. After the hook, not before, because an enrichment
	// may set the very field the condition reads (`owner`, say) — and the row
	// that gets written is the one the condition must have approved.
	if !grants.Allow(rec) {
		return nil, authz.ErrPermissionDenied
	}

	translations, _, err := c.translationsFrom(rec)
	if err != nil {
		return nil, err
	}

	id, err := recordID(rec)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	table := TableName(c.schema.ObjectName)

	var result Record
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		cols := []string{"id", "tenant_id"}
		vals := []any{id, string(tenant)}
		customFields := map[string]any{}
		if blob := translations.blobValue(); blob != nil {
			customFields[translationsBlobKey] = blob
		}
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
			// A generated table declares no unique constraint but its primary
			// key (GenerateDDL), so a unique violation here is that key and
			// nothing else — which only a caller-supplied id can collide with.
			if storage.IsUniqueViolation(err) {
				return fmt.Errorf("%w", ErrIDTaken)
			}
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
		// Hand back the parsed shape, not whatever the caller passed, so a
		// Record read back from Create looks like one read back from Get.
		setTranslations(result, translations)
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

// ListPage returns up to limit non-archived records ordered by id, starting
// after the given id ("" for the first page). It is what hydration reads
// (WP-2.2a): List loads a tenant's entire table into memory on both ends,
// which is fine for a screen and not for seeding a replica of a real book.
//
// Ordering is by id rather than created_at because ids are UUIDv7 — already
// time-ordered, and unique, so a page boundary cannot straddle two rows
// sharing a timestamp and silently drop one.
//
// Archived rows are excluded: a fresh replica never held them, so it does not
// need to be told they are gone. Rows archived *after* the snapshot's cursor
// arrive through the feed, where GetMany does convey them (see its doc).
func (c *CRUD) ListPage(ctx context.Context, db *storage.DB, tenant tenancy.ID, after string, limit int) ([]Record, error) {
	if _, err := authz.Authorize(ctx, db, c.schema.ObjectName, "read"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("metadata: limit must be positive")
	}

	var records []Record
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		query := fmt.Sprintf(`SELECT %s FROM %s
			WHERE tenant_id = ? AND archived_at IS NULL AND id > ?
			ORDER BY id ASC LIMIT ?`,
			selectColumns(c.schema), TableName(c.schema.ObjectName))
		rows, err := tx.QueryContext(ctx, db.Rebind(query), string(tenant), after, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		var list []Record
		for rows.Next() {
			rec, err := scanRecord(rows, c.schema)
			if err != nil {
				return err
			}
			list = append(list, rec)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		records = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetMany returns the records with the given ids, **including archived ones**,
// requiring the "read" permission.
//
// The archived rows are the point. This is what resolves change-feed pointers
// into rows (WP-2.2a), and a replica that already holds a row and is told
// nothing when it is deleted keeps it forever — a divergence no amount of
// further syncing repairs, which is precisely what INV-S3 exists to catch. A
// returned record carries archived_at when it is deleted, and the caller
// removes it locally.
//
// Unknown ids are simply absent from the result; a pointer to a row that no
// longer exists at all is not an error to the reader.
func (c *CRUD) GetMany(ctx context.Context, db *storage.DB, tenant tenancy.ID, ids []string) ([]Record, error) {
	if _, err := authz.Authorize(ctx, db, c.schema.ObjectName, "read"); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, string(tenant))
	for _, id := range ids {
		args = append(args, id)
	}

	var records []Record
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND id IN (%s) ORDER BY id ASC`,
			selectColumns(c.schema), TableName(c.schema.ObjectName), placeholders)
		rows, err := tx.QueryContext(ctx, db.Rebind(query), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		var list []Record
		for rows.Next() {
			rec, err := scanRecord(rows, c.schema)
			if err != nil {
				return err
			}
			list = append(list, rec)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		records = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
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

		var list []Record
		for rows.Next() {
			rec, err := scanRecord(rows, c.schema)
			if err != nil {
				return err
			}
			list = append(list, rec)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		records = list
		return nil
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
	actor, grants, err := authz.AuthorizeGrants(ctx, db, c.schema.ObjectName, "update")
	if err != nil {
		return nil, err
	}
	// Until WP-1.11 this path ran no validation at all — not even the
	// required-field check Create had — so a PUT could null a required column
	// or write a bool into an int one. partial=true: only touched keys.
	changes, err = c.validated(changes, true)
	if err != nil {
		return nil, err
	}
	changes, err = c.runBefore(ctx, tenant, VerbUpdate, changes, true)
	if err != nil {
		return nil, err
	}
	newTranslations, translationsTouched, err := c.translationsFrom(changes)
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
		// Second stage of the gate (WP-3.3a), inside the transaction that will
		// do the writing: the row a condition approved is the row that gets
		// updated, with no window between the check and the write.
		//
		// ponytail: the condition is evaluated against the *current* row only —
		// Postgres RLS's USING half, not its WITH CHECK half. So a condition
		// says which rows you may touch, not what they may become; a rule that
		// stops an owner reassigning their record to someone else is a
		// validation rule or a hook today. Add the post-state check here if a
		// tenant needs the WITH CHECK semantics.
		if !grants.Allow(current) {
			return authz.ErrPermissionDenied
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
		// Translations share the blob with overlay fields, so they are seeded
		// from the current row for the same reason: an update that touches one
		// overlay field re-marshals the whole blob and would otherwise drop
		// every translation on the row.
		if stored, ok := current[TranslationsKey].(Translations); ok {
			if blob := stored.blobValue(); blob != nil {
				customFields[translationsBlobKey] = blob
			}
		}
		if translationsTouched {
			diff[TranslationsKey] = map[string]any{"old": current[TranslationsKey], "new": newTranslations}
			customTouched = true
			if blob := newTranslations.blobValue(); blob != nil {
				customFields[translationsBlobKey] = blob
			} else {
				delete(customFields, translationsBlobKey)
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
		if translationsTouched {
			setTranslations(merged, newTranslations)
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
	actor, grants, err := authz.AuthorizeGrants(ctx, db, c.schema.ObjectName, "delete")
	if err != nil {
		return err
	}
	// A delete hook may veto and nothing else — there is no record to enrich,
	// so the returned one is deliberately discarded.
	if _, err := c.runBefore(ctx, tenant, VerbDelete, Record{"id": id}, true); err != nil {
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
		// Second stage of the gate (WP-3.3a), inside the writing transaction.
		if !grants.Allow(current) {
			return authz.ErrPermissionDenied
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
	setTranslations(rec, translationsFromBlob(customFields))
	return rec, nil
}

// setTranslations puts t under the reserved key, or removes the key when there
// is nothing to say — an empty translations object on every untranslated record
// is noise in every API response.
func setTranslations(rec Record, t Translations) {
	if len(t) == 0 {
		delete(rec, TranslationsKey)
		return
	}
	rec[TranslationsKey] = t
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

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Schema-conformance reporting for `lasterp doctor` — the detection half of
// WP-1.11 (INV-T5).
//
// Validation only binds writes made after it lands. Rows written before it
// keep whatever they hold, and WP-1.11 deliberately does not rewrite them: no
// backfill can know what `type: "banana"` was meant to be, and a guess writes a
// wrong number into a chart of accounts. Coercing such a row to a valid value
// would be the same silent misstatement modules/reporting's `unclassified`
// bucket exists to prevent.
//
// So the rows stay, readable and editable, and this is what finds them. Only
// Account.type had any visibility before (via that bucket); Contact.kind had
// none at all.
//
// It reports without changing `doctor`'s exit status. That exit code means
// "database role separation is not in effect" and is used as a deploy gate;
// a deployment holding one legacy account is misconfigured data, not an
// unhealthy process, and conflating the two teaches operators to disable the
// gate.

// FieldConformance counts one enum field's values that fall outside the option
// set its schema declares.
type FieldConformance struct {
	Object string `json:"object"`
	Field  string `json:"field"`
	Tenant string `json:"tenant"`
	// Value is the offending stored value, and Count how many rows hold it.
	Value string `json:"value"`
	Count int    `json:"count"`
}

// SchemaConformance is what `lasterp doctor` reports about stored data, as
// opposed to Posture, which reports about the connection.
type SchemaConformance struct {
	// Checked is how many (tenant, object, enum field) triples were scanned, so
	// a clean report is distinguishable from a report that scanned nothing.
	//
	// Per tenant rather than per object since WP-3.2c: the effective schema is
	// the tenant's, so two tenants of the same object can have different enum
	// fields with different option sets, and one number covering both would be
	// counting something that no longer exists.
	Checked int `json:"checked_fields"`
	// Findings is empty when every stored value conforms.
	Findings []FieldConformance `json:"findings,omitempty"`
	// Note explains a scan that could not run to completion rather than
	// letting an empty Findings list read as "clean".
	Note string `json:"note,omitempty"`
}

// DiagnoseSchemaConformance scans every registered CRUD object's enum columns
// for values outside their declared options.
//
// It reads the registered schemas out of object_schemas rather than taking a
// list of modules, so it stays module-agnostic: an object shipped by a module
// this binary does not import is still scanned, and no module needs to export
// its schema to be covered.
//
// The scan runs per tenant inside tenancy.WithTenant rather than against the
// bare connection, because obj_* tables carry FORCE ROW LEVEL SECURITY: a
// tenant-less scan under the hardened posture returns zero rows and would
// report "clean" for a database full of violations. Satisfying RLS is the only
// honest way to do this; bypassing it would make the diagnostic a second
// INV-T1 hole.
func DiagnoseSchemaConformance(ctx context.Context, db *storage.DB) (SchemaConformance, error) {
	var out SchemaConformance

	schemas, unparsed, err := registeredCRUDSchemas(ctx, db)
	if err != nil {
		return out, err
	}
	// A definition stored by an older binary can fail today's parser — WP-1.11
	// made `options` mandatory on an enum, so a pre-WP-1.11 row does not parse.
	// That is not a broken upgrade (Register parses the *current* YAML and
	// ApplyDDL diffs against the JSON snapshot, never re-parsing old YAML), but
	// `doctor` can be pointed at a database no new binary has booted against
	// yet. Skipping and saying so beats refusing to report anything.
	for _, note := range unparsed {
		out.Note = strings.TrimSpace(out.Note + " " + note)
	}
	tenants, err := allTenants(ctx, db)
	if err != nil {
		return out, err
	}
	if len(tenants) == 0 {
		out.Note = strings.TrimSpace(out.Note + " no tenants exist yet; nothing to scan")
		return out, nil
	}

	// The schema is resolved per tenant, not once (WP-3.2c). Two things depend
	// on it, and each is a class of violation an object-level scan cannot see:
	// a tenant that *narrowed* a core enum has rows outside its own set and
	// inside core's, and a tenant whose overlay *added* an enum has a field the
	// core object does not mention at all.
	for _, core := range schemas {
		for _, tenant := range tenants {
			eff, err := metadata.Resolve(ctx, db, tenant, core)
			if err != nil {
				out.Note = strings.TrimSpace(fmt.Sprintf("%s %s was not scanned for tenant %s: %v.",
					out.Note, core.ObjectName, tenant, err))
				continue
			}
			for _, f := range eff.Fields {
				if f.Type != metadata.FieldEnum {
					continue
				}
				out.Checked++
				findings, err := nonConformingValues(ctx, db, tenant, core.ObjectName, f)
				if err != nil {
					return SchemaConformance{}, err
				}
				out.Findings = append(out.Findings, findings...)
			}
		}
	}
	sort.Slice(out.Findings, func(i, j int) bool {
		a, b := out.Findings[i], out.Findings[j]
		if a.Object != b.Object {
			return a.Object < b.Object
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		return a.Value < b.Value
	})
	return out, nil
}

// nonConformingValues groups one tenant's out-of-set values for one enum field.
func nonConformingValues(ctx context.Context, db *storage.DB, tenant tenancy.ID, object string, f metadata.Field) ([]FieldConformance, error) {
	// The field name comes from a schema this process validated — Field.validate
	// bounds it to `^[a-z][a-z0-9_]*$`, which is what makes interpolating it
	// safe now that an overlay can supply one — and the option list is bound as
	// parameters. Table and column cannot be parameterized in any dialect.
	//
	// An overlay field has no column of its own: it lives in the custom_fields
	// blob (ADR-006), so it is read through the dialect's JSON accessor. Same
	// query otherwise — a value outside the option set is the same finding
	// wherever it is stored.
	expr := f.Name
	if f.FromOverlay {
		expr = jsonFieldExpr(db.Dialect, f.Name)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(f.Options)), ", ")
	query := fmt.Sprintf(
		`SELECT %s, COUNT(*) FROM %s WHERE tenant_id = ? AND %s IS NOT NULL AND %s NOT IN (%s) GROUP BY %s`,
		expr, metadata.TableName(object), expr, expr, placeholders, expr)

	args := make([]any, 0, len(f.Options)+1)
	args = append(args, string(tenant))
	for _, opt := range f.Options {
		args = append(args, opt)
	}

	var out []FieldConformance
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(query), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []FieldConformance
		for rows.Next() {
			finding := FieldConformance{Object: object, Field: f.Name, Tenant: string(tenant)}
			if err := rows.Scan(&finding.Value, &finding.Count); err != nil {
				return err
			}
			list = append(list, finding)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("app: scan %s.%s for tenant %s: %w", object, f.Name, tenant, err)
	}
	return out, nil
}

// jsonFieldExpr renders "read this key out of the custom_fields blob, as text"
// in the dialect's own spelling. Both return SQL NULL for an absent key, which
// is what makes the caller's `IS NOT NULL` filter mean "the row has a value for
// this overlay field" in either.
//
// The name is an identifier by construction (fieldNameRE), so there is nothing
// to escape; binding it as a parameter would work on both dialects but split one
// readable statement across four positional arguments that must stay in order.
func jsonFieldExpr(dialect storage.Dialect, name string) string {
	if dialect == storage.Postgres {
		return fmt.Sprintf(`(custom_fields::jsonb ->> '%s')`, name)
	}
	return fmt.Sprintf(`json_extract(custom_fields, '$.%s')`, name)
}

// registeredCRUDSchemas returns the *core* definition of every core-layer object
// that has a generated table, at its highest registered version, plus a note
// for each object whose stored definition this binary cannot parse.
//
// Core, not effective: the effective schema is per tenant since WP-3.2c, so the
// merge happens in the caller's tenant loop where the overlays are known.
//
// An unparseable definition is reported, never fatal: the caller is a
// diagnostic, and one stale row must not take the whole report offline.
func registeredCRUDSchemas(ctx context.Context, db *storage.DB) ([]*metadata.Object, []string, error) {
	names, err := registeredObjectNames(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	var out []*metadata.Object
	var unparsed []string
	for _, name := range names {
		stored, err := metadata.LoadObjectSchema(ctx, db, "", metadata.LayerCore, name)
		if err != nil {
			return nil, nil, fmt.Errorf("app: load schema %q: %w", name, err)
		}
		object, err := metadata.ParseObject(stored.Definition)
		if err != nil {
			unparsed = append(unparsed, fmt.Sprintf(
				"%s v%d was not scanned: this binary cannot parse its stored definition (%v). "+
					"It was probably written by an older version; run `lasterp migrate` with this binary.",
				name, stored.Version, err))
			continue
		}
		if object.Persistence != metadata.PersistenceCRUD {
			// Event-sourced objects have no generated table to scan; their
			// data is the event log.
			continue
		}
		out = append(out, object)
	}
	return out, unparsed, nil
}

func registeredObjectNames(ctx context.Context, db *storage.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, db.Rebind(
		`SELECT DISTINCT name FROM object_schemas WHERE layer = ? ORDER BY name`), string(metadata.LayerCore))
	if err != nil {
		return nil, fmt.Errorf("app: list registered objects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("app: scan registered object: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("app: list registered objects: %w", err)
	}
	return names, nil
}

// allTenants lists every tenant. The tenants table is the tenant registry
// itself and carries no RLS policy by design (ADR-005: "tenants themselves are
// not tenant-scoped"), which is what makes a cross-tenant scan possible without
// a privileged connection.
func allTenants(ctx context.Context, db *storage.DB) ([]tenancy.ID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("app: list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []tenancy.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("app: scan tenant: %w", err)
		}
		out = append(out, tenancy.ID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("app: list tenants: %w", err)
	}
	return out, nil
}

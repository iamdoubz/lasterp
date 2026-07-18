// SPDX-License-Identifier: AGPL-3.0-only

package tax

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrInvalidLevel is returned by SaveJurisdiction for a level outside the closed
// set.
var ErrInvalidLevel = errors.New("tax: invalid jurisdiction level")

var validLevels = map[string]bool{
	LevelCountry: true, LevelState: true, LevelProvince: true,
}

// Jurisdiction is a tax jurisdiction: a code plus its human name, ISO country,
// and level.
type Jurisdiction struct {
	Code    string
	Name    string
	Country string
	Level   string
}

// SaveJurisdiction records j under tenant (use GlobalTenant for pack rows). It
// is reference data, isolated by RLS/tenant predicate like fx_rates — no authz
// in v1; the write path adds it when a tax API surface lands (WP-1.4/1.5).
func SaveJurisdiction(ctx context.Context, db *storage.DB, tenant tenancy.ID, j Jurisdiction) error {
	if j.Code == "" || j.Name == "" || j.Country == "" {
		return errors.New("tax: jurisdiction code, name, and country are required")
	}
	if !validLevels[j.Level] {
		return fmt.Errorf("%w: %q", ErrInvalidLevel, j.Level)
	}
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO tax_jurisdictions (tenant_id, code, name, country, level, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?)`),
			string(tenant), j.Code, j.Name, j.Country, j.Level, time.Now().UTC())
		return err
	})
}

// ListJurisdictions returns the jurisdictions visible to tenant: its own plus
// the shared global ones, ordered by code. A tenant override (same code) beats
// the global row.
func ListJurisdictions(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Jurisdiction, error) {
	var out []Jurisdiction
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// The (tenant_id = ? OR '') predicate is the isolation on SQLite (no RLS)
		// and redundant-but-harmless under Postgres RLS. Dedup by code, preferring
		// the tenant's own row over the global one.
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT code, name, country, level FROM tax_jurisdictions t
			WHERE (tenant_id = ? OR tenant_id = '')
			      AND (tenant_id = ? OR NOT EXISTS (
			          SELECT 1 FROM tax_jurisdictions o
			          WHERE o.code = t.code AND o.tenant_id = ?))
			ORDER BY code`),
			string(tenant), string(tenant), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var j Jurisdiction
			if err := rows.Scan(&j.Code, &j.Name, &j.Country, &j.Level); err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	return out, err
}

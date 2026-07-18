// SPDX-License-Identifier: AGPL-3.0-only

package tax

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrRateNotFound is returned when no rate is on file for a
// (jurisdiction, category) as of the requested date.
var ErrRateNotFound = errors.New("tax: no tax rate on file")

// Rate is one effective-dated tax rate: Rate (an exact decimal-string fraction,
// e.g. "0.20" for 20%) applies in Jurisdiction for Category on/after AsOf, with
// the given Rounding rule.
type Rate struct {
	Jurisdiction string
	Category     string
	Rate         string // exact decimal fraction, e.g. "0.0825"
	Rounding     string // RoundHalfEven (default) or RoundHalfUp
	AsOf         time.Time
	Name         string
	Provider     string
}

// ResolvedRate is a rate looked up for a date: the exact fraction plus its
// rounding rule.
type ResolvedRate struct {
	Rate     *big.Rat
	Rounding string
}

// SaveRate records r under tenant (use GlobalTenant for pack rows), validating
// the rate decimal and rounding rule. Like fx_rates, it is reference data with
// no authz in v1 — isolation is DB-enforced (RLS/tenant predicate).
func SaveRate(ctx context.Context, db *storage.DB, tenant tenancy.ID, r Rate) error {
	if r.Jurisdiction == "" || r.Category == "" {
		return errors.New("tax: rate jurisdiction and category are required")
	}
	rat, ok := new(big.Rat).SetString(r.Rate)
	if !ok {
		return fmt.Errorf("tax: invalid rate %q", r.Rate)
	}
	if rat.Sign() < 0 {
		return fmt.Errorf("tax: rate %q must be non-negative", r.Rate)
	}
	if _, err := roundingMode(r.Rounding); err != nil {
		return err
	}
	if r.AsOf.IsZero() {
		return errors.New("tax: rate AsOf date is required")
	}
	rounding := r.Rounding
	if rounding == "" {
		rounding = RoundHalfEven
	}
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO tax_rates (tenant_id, jurisdiction, category, rate, rounding, as_of, name, provider, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), r.Jurisdiction, r.Category, r.Rate, rounding,
			r.AsOf.UTC().Format(dateLayout), r.Name, r.Provider, time.Now().UTC())
		return err
	})
}

// RateAsOf returns the tax rate in effect on date for (jurisdiction, category):
// the latest rate with as_of <= date, preferring the tenant's own override over
// the global row. ErrRateNotFound if none is on file.
func RateAsOf(ctx context.Context, db *storage.DB, tenant tenancy.ID, jurisdiction, category string, date time.Time) (ResolvedRate, error) {
	var rateStr, rounding string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// The (tenant_id = ? OR '') predicate is the isolation on SQLite (no RLS)
		// and redundant-but-harmless under Postgres RLS. The CASE prefers the
		// tenant's own override over the global row.
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT rate, rounding FROM tax_rates
			WHERE jurisdiction = ? AND category = ? AND as_of <= ?
			      AND (tenant_id = ? OR tenant_id = '')
			ORDER BY (CASE WHEN tenant_id = ? THEN 0 ELSE 1 END), as_of DESC
			LIMIT 1`),
			jurisdiction, category, date.UTC().Format(dateLayout), string(tenant), string(tenant))
		scanErr := row.Scan(&rateStr, &rounding)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s as of %s", ErrRateNotFound, jurisdiction, category, date.UTC().Format(dateLayout))
		}
		return scanErr
	})
	if err != nil {
		return ResolvedRate{}, err
	}
	rat, ok := new(big.Rat).SetString(rateStr)
	if !ok {
		return ResolvedRate{}, fmt.Errorf("tax: corrupt stored rate %q", rateStr)
	}
	return ResolvedRate{Rate: rat, Rounding: rounding}, nil
}

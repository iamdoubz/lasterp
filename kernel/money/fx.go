// SPDX-License-Identifier: AGPL-3.0-only

package money

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

// GlobalTenant is the sentinel tenant under which shared, provider-sourced
// rates (e.g. ECB) are stored. Every tenant can read them; a tenant's own
// override rows use its real tenant_id and win over the global rate (ADR-013;
// WP-1.1 decisions, decision 6). Same shared-sentinel pattern kernel/metadata
// uses for core schemas.
const GlobalTenant tenancy.ID = ""

// dateLayout is the effective-date format stored in fx_rates.as_of. A plain
// YYYY-MM-DD string sorts chronologically and compares identically on Postgres
// and SQLite, sidestepping any driver timestamp-format differences.
const dateLayout = "2006-01-02"

// ErrRateNotFound is returned when no rate (direct or inverse) is on file for a
// pair as of the requested date.
var ErrRateNotFound = errors.New("money: no FX rate on file")

// Rate is one effective-dated exchange rate: Rate units of Quote per unit of
// Base, effective on/after AsOf.
type Rate struct {
	Base     string
	Quote    string
	Rate     string // exact decimal, e.g. "1.0873"
	AsOf     time.Time
	Provider string
}

// SaveRate records r under tenant (use GlobalTenant for provider rates),
// validating both currencies and the rate decimal.
func SaveRate(ctx context.Context, db *storage.DB, tenant tenancy.ID, r Rate) error {
	bc, err := Lookup(r.Base)
	if err != nil {
		return err
	}
	qc, err := Lookup(r.Quote)
	if err != nil {
		return err
	}
	if _, ok := new(big.Rat).SetString(r.Rate); !ok {
		return fmt.Errorf("money: invalid rate %q", r.Rate)
	}
	if r.AsOf.IsZero() {
		return errors.New("money: rate AsOf date is required")
	}
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO fx_rates (tenant_id, base, quote, rate, as_of, provider, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), bc.Code, qc.Code, r.Rate, r.AsOf.UTC().Format(dateLayout), r.Provider, time.Now().UTC())
		return err
	})
}

// RateAsOf returns the exchange rate (units of quote per unit of base) in
// effect on date: the latest rate with as_of <= date, preferring tenant's own
// override over the global rate. base==quote returns 1; if only the reverse
// pair is on file, its inverse is returned. ErrRateNotFound otherwise.
func RateAsOf(ctx context.Context, db *storage.DB, tenant tenancy.ID, base, quote string, date time.Time) (*big.Rat, error) {
	bc, err := Lookup(base)
	if err != nil {
		return nil, err
	}
	qc, err := Lookup(quote)
	if err != nil {
		return nil, err
	}
	if bc.Code == qc.Code {
		return big.NewRat(1, 1), nil
	}

	switch r, err := lookupRate(ctx, db, tenant, bc.Code, qc.Code, date); {
	case err == nil:
		return r, nil
	case !errors.Is(err, ErrRateNotFound):
		return nil, err
	}

	switch r, err := lookupRate(ctx, db, tenant, qc.Code, bc.Code, date); {
	case err == nil:
		return new(big.Rat).Inv(r), nil
	case !errors.Is(err, ErrRateNotFound):
		return nil, err
	}

	return nil, fmt.Errorf("%w: %s/%s as of %s", ErrRateNotFound, bc.Code, qc.Code, date.UTC().Format(dateLayout))
}

func lookupRate(ctx context.Context, db *storage.DB, tenant tenancy.ID, base, quote string, date time.Time) (*big.Rat, error) {
	var rateStr string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// The (tenant_id = ? OR '') predicate is the isolation on SQLite (no
		// RLS) and redundant-but-harmless under Postgres RLS. The CASE prefers
		// the tenant's own override over the global row.
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT rate FROM fx_rates
			WHERE base = ? AND quote = ? AND as_of <= ?
			      AND (tenant_id = ? OR tenant_id = '')
			ORDER BY (CASE WHEN tenant_id = ? THEN 0 ELSE 1 END), as_of DESC
			LIMIT 1`),
			base, quote, date.UTC().Format(dateLayout), string(tenant), string(tenant))
		scanErr := row.Scan(&rateStr)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrRateNotFound
		}
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	r, ok := new(big.Rat).SetString(rateStr)
	if !ok {
		return nil, fmt.Errorf("money: corrupt stored rate %q", rateStr)
	}
	return r, nil
}

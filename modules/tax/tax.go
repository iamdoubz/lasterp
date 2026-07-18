// SPDX-License-Identifier: AGPL-3.0-only

// Package tax is the WP-1.3 tax engine (ADR-013): jurisdictions and effective-
// dated rates as reference data, plus a pure document tax calculation. v1 ships
// local editable tax tables with community seed packs (US state sales, EU VAT);
// commercial rate providers (Avalara, Vertex, …) are optional connectors later.
//
// The engine computes; it does not post. Invoicing (WP-1.4) posts the computed
// tax amounts to the ledger through its GL template. All money math goes through
// kernel/money (INV-F4 — no floats). Reference-data isolation is DB-enforced
// (RLS split policies, the fx_rates pattern) so a tenant reads global + its own
// rows only (INV-T1). Modules import kernel/* only, never other modules
// (CLAUDE.md).
package tax

import (
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// GlobalTenant is the sentinel tenant under which shared, pack/provider-sourced
// jurisdictions and rates are stored. Every tenant reads them; a tenant's own
// override rows use its real tenant_id and win over the global row (ADR-013).
// Same shared-sentinel pattern as kernel/money.GlobalTenant.
const GlobalTenant tenancy.ID = ""

// dateLayout is the effective-date format stored in tax_rates.as_of. A plain
// YYYY-MM-DD string sorts chronologically and compares identically on Postgres
// and SQLite (the WP-1.1 fx lesson).
const dateLayout = "2006-01-02"

// Jurisdiction levels (closed set). A jurisdiction is a country, or a
// sub-national state/province.
const (
	LevelCountry  = "country"
	LevelState    = "state"
	LevelProvince = "province"
)

// Tax rate categories are open strings the seed packs define; these are the
// common ones. VAT regimes use standard/reduced/zero/exempt; US sales tax uses
// sales.
const (
	CategoryStandard = "standard"
	CategoryReduced  = "reduced"
	CategoryZero     = "zero"
	CategoryExempt   = "exempt"
	CategorySales    = "sales"
)

// Rounding rule names stored in tax_rates.rounding (the "rule as data"). Some
// jurisdictions mandate half-up; the default is banker's rounding.
const (
	RoundHalfEven = "half_even"
	RoundHalfUp   = "half_up"
)

// roundingMode maps a stored rounding rule name to a money.RoundingMode. An
// empty string defaults to half-even (the column default).
func roundingMode(name string) (money.RoundingMode, error) {
	switch name {
	case "", RoundHalfEven:
		return money.HalfEven, nil
	case RoundHalfUp:
		return money.HalfUp, nil
	default:
		return 0, fmt.Errorf("tax: unknown rounding rule %q", name)
	}
}

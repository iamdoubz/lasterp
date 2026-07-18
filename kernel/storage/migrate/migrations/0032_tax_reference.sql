-- WP-1.3 tax engine v1: jurisdictions + effective-dated rates as reference data
-- (ADR-013). Both tables follow the WP-1.1 fx_rates pattern: provider/seed rows
-- use the shared sentinel tenant_id '' (modules/tax.GlobalTenant); a tenant's
-- own override rows carry its real tenant_id and win over the global row. These
-- are mutable reference data (rates get corrected/re-seeded) — history is the
-- effective-dating, not append-only immutability.

-- Jurisdictions are static reference (no effective-dating): a code plus its
-- human name, ISO country, and level (country/state/province).
CREATE TABLE tax_jurisdictions (
	tenant_id TEXT NOT NULL,
	code TEXT NOT NULL,
	name TEXT NOT NULL,
	country TEXT NOT NULL,
	level TEXT NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_tax_jurisdictions_lookup ON tax_jurisdictions (tenant_id, code);

-- Effective-dated tax rates. rate is an exact decimal-string fraction
-- ("0.20" = 20%); category is an open string ('standard'/'reduced'/'zero'/
-- 'exempt'/'sales'/…) the seed packs define; rounding is the "rule as data"
-- ('half_even' default, 'half_up' where a jurisdiction mandates it); as_of is a
-- plain YYYY-MM-DD effective-from string (sorts/compares identically on Postgres
-- and SQLite); recorded_at is the audit timestamp (UTC).
CREATE TABLE tax_rates (
	tenant_id TEXT NOT NULL,
	jurisdiction TEXT NOT NULL,
	category TEXT NOT NULL,
	rate TEXT NOT NULL,
	rounding TEXT NOT NULL DEFAULT 'half_even',
	as_of TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	recorded_at TIMESTAMPTZ NOT NULL
);

-- tenant_id first (ADR-005), then the jurisdiction+category+date the effective-
-- dated lookup filters and orders on.
CREATE INDEX idx_tax_rates_lookup ON tax_rates (tenant_id, jurisdiction, category, as_of);

-- WP-1.1 money core: effective-dated FX rate store (ADR-013).
-- Rates are stored historically (rate-as-of-transaction-date is an accounting
-- requirement). Provider/global rates use the shared sentinel tenant_id ''
-- (kernel/money.GlobalTenant, same pattern as core object_schemas); a tenant's
-- own override rows carry its real tenant_id and win over the global rate.
-- as_of is a plain YYYY-MM-DD string so it sorts and compares identically on
-- Postgres and SQLite; recorded_at is the audit timestamp (UTC).
CREATE TABLE fx_rates (
	tenant_id TEXT NOT NULL,
	base TEXT NOT NULL,
	quote TEXT NOT NULL,
	rate TEXT NOT NULL,
	as_of TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	recorded_at TIMESTAMPTZ NOT NULL
);

-- tenant_id first (ADR-005), then the pair + date the effective-dated lookup
-- filters and orders on.
CREATE INDEX idx_fx_rates_lookup ON fx_rates (tenant_id, base, quote, as_of);

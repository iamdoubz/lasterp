-- WP-1.2 PR-B: the trial-balance read model. A projection of the ledger.entry.
-- posted event stream — rebuildable from the log (INV-E5), so it is derived
-- state, never a source of truth. net_minor is Σdebits − Σcredits for the
-- (account, currency) in minor units (positive = net debit). Since every entry
-- balances (INV-F1), Σ net_minor across a tenant's accounts (per currency) is 0.
CREATE TABLE ledger_balances (
	tenant_id TEXT NOT NULL,
	account_id TEXT NOT NULL,
	currency TEXT NOT NULL,
	net_minor BIGINT NOT NULL,
	PRIMARY KEY (tenant_id, account_id, currency)
);

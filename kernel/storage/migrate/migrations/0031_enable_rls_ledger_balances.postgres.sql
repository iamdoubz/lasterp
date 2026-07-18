-- Tenant isolation for the trial-balance projection (commandment 8).
ALTER TABLE ledger_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_balances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_ledger_balances ON ledger_balances
	USING (tenant_id = current_setting('app.tenant_id', true));

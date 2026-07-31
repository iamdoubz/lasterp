-- Tenant isolation for the projection cursor (commandment 8).
ALTER TABLE ledger_balance_cursor ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_balance_cursor FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_ledger_balance_cursor ON ledger_balance_cursor
	USING (tenant_id = current_setting('app.tenant_id', true));

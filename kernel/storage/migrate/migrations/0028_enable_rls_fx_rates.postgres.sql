-- RLS for fx_rates (commandment 8: the database enforces tenant isolation).
-- Reads: your own tenant's overrides plus the shared global ('' sentinel)
-- rates. Writes: only rows owned by the current tenant context — a tenant
-- cannot write another tenant's rows, and the global rows can be written only
-- under the sentinel context (app.tenant_id = ''), closing the hole where a
-- tenant could poison the shared rate every other tenant reads.
ALTER TABLE fx_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rates FORCE ROW LEVEL SECURITY;

CREATE POLICY fx_rates_select ON fx_rates FOR SELECT
	USING (tenant_id = current_setting('app.tenant_id', true) OR tenant_id = '');

CREATE POLICY fx_rates_modify ON fx_rates FOR ALL
	USING (tenant_id = current_setting('app.tenant_id', true))
	WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

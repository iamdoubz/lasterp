-- RLS for the tax reference tables (commandment 8: the database enforces tenant
-- isolation). Same split-policy shape as fx_rates: reads see your own tenant's
-- overrides plus the shared global ('' sentinel) rows; writes touch only rows
-- owned by the current tenant context, so a tenant cannot write another tenant's
-- rows and the global rows can be written only under the sentinel context
-- (app.tenant_id = '').
ALTER TABLE tax_jurisdictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_jurisdictions FORCE ROW LEVEL SECURITY;

CREATE POLICY tax_jurisdictions_select ON tax_jurisdictions FOR SELECT
	USING (tenant_id = current_setting('app.tenant_id', true) OR tenant_id = '');

CREATE POLICY tax_jurisdictions_modify ON tax_jurisdictions FOR ALL
	USING (tenant_id = current_setting('app.tenant_id', true))
	WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE tax_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_rates FORCE ROW LEVEL SECURITY;

CREATE POLICY tax_rates_select ON tax_rates FOR SELECT
	USING (tenant_id = current_setting('app.tenant_id', true) OR tenant_id = '');

CREATE POLICY tax_rates_modify ON tax_rates FOR ALL
	USING (tenant_id = current_setting('app.tenant_id', true))
	WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

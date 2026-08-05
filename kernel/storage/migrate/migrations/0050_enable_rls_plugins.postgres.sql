-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE plugins ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugins FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_plugins ON plugins
	USING (tenant_id = current_setting('app.tenant_id', true));

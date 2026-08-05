-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE plugin_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugin_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_plugin_deliveries ON plugin_deliveries
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE plugin_dead_letters ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugin_dead_letters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_plugin_dead_letters ON plugin_dead_letters
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE plugin_kv ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugin_kv FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_plugin_kv ON plugin_kv
	USING (tenant_id = current_setting('app.tenant_id', true));

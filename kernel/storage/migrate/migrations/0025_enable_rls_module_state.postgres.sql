-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE module_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE module_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_module_state ON module_state
	USING (tenant_id = current_setting('app.tenant_id', true));

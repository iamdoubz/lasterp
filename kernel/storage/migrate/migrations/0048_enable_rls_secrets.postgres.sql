-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_secrets ON secrets
	USING (tenant_id = current_setting('app.tenant_id', true));

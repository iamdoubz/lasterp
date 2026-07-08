ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_audit_log ON audit_log
	USING (tenant_id = current_setting('app.tenant_id', true));

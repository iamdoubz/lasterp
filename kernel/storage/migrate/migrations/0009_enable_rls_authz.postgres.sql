-- FORCE is required in addition to ENABLE: see 0005_enable_rls_users.postgres.sql.
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_roles ON roles
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE role_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_permissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_role_permissions ON role_permissions
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE user_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_user_roles ON user_roles
	USING (tenant_id = current_setting('app.tenant_id', true));

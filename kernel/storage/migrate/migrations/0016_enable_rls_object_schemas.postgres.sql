-- Core-layer schemas (tenant_id = '') are shared across every tenant, so
-- the policy admits them unconditionally in addition to the usual
-- tenant-scoped rows — otherwise no tenant's session could ever see a
-- core object definition under RLS. FORCE per the WP-0.3 lesson (table
-- owners bypass RLS unless forced).
ALTER TABLE object_schemas ENABLE ROW LEVEL SECURITY;
ALTER TABLE object_schemas FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_object_schemas ON object_schemas
	USING (layer = 'core' OR tenant_id = current_setting('app.tenant_id', true));

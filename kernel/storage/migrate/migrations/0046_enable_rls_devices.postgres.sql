-- Tenant isolation for devices (commandment 8, INV-T1). A device row names a
-- user and a machine that holds tenant data offline, so cross-tenant visibility
-- here would leak the shape of another tenant's estate — and the wipe control
-- on this table must never be reachable across a tenant boundary.
ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_devices ON devices
	USING (tenant_id = current_setting('app.tenant_id', true));

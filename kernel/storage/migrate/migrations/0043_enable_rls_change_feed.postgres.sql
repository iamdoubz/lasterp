-- Tenant isolation for the change feed (commandment 8, INV-T1). The feed is
-- the one surface whose entire job is to hand data to a device that then holds
-- it offline, so a leak here is a leak that leaves the building.
ALTER TABLE change_feed ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_feed FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_change_feed ON change_feed
	USING (tenant_id = current_setting('app.tenant_id', true));

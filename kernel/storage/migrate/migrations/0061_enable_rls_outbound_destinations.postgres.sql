-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE outbound_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbound_destinations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_outbound_destinations ON outbound_destinations
	USING (tenant_id = current_setting('app.tenant_id', true));

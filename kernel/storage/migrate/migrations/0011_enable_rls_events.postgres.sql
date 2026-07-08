-- See kernel/tenancy's WP-0.3 notes: FORCE is required in addition to
-- ENABLE, or the role that ran migrations (its owner) bypasses RLS.
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_events ON events
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE stream_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE stream_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_stream_snapshots ON stream_snapshots
	USING (tenant_id = current_setting('app.tenant_id', true));

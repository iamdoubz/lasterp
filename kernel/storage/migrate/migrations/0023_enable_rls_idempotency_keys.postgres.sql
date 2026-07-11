ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_idempotency_keys ON idempotency_keys
	USING (tenant_id = current_setting('app.tenant_id', true));

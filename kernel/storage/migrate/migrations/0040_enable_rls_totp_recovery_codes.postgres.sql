-- Tenant isolation for recovery codes (commandment 8). A code minted in tenant
-- A must not authenticate anyone in tenant B (INV-T1).
ALTER TABLE totp_recovery_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE totp_recovery_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_totp_recovery_codes ON totp_recovery_codes
	USING (tenant_id = current_setting('app.tenant_id', true));

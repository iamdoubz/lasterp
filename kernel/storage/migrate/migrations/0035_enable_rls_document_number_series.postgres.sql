-- FORCE so the migration-runner (table owner) role does not bypass RLS; the
-- series counter is plain per-tenant state (no global sentinel), so a single
-- tenant_isolation policy covers read and write.
ALTER TABLE document_number_series ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_number_series FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_document_number_series ON document_number_series
	USING (tenant_id = current_setting('app.tenant_id', true));

-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE automations ENABLE ROW LEVEL SECURITY;
ALTER TABLE automations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_automations ON automations
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE automation_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_automation_cursors ON automation_cursors
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE automation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_automation_runs ON automation_runs
	USING (tenant_id = current_setting('app.tenant_id', true));

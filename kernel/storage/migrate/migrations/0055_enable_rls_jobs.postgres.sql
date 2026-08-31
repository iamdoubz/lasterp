-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_jobs ON jobs
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE job_dead_letters ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_dead_letters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_job_dead_letters ON job_dead_letters
	USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE job_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_job_schedules ON job_schedules
	USING (tenant_id = current_setting('app.tenant_id', true));

-- See kernel/tenancy: FORCE is required in addition to ENABLE so the role
-- that ran migrations (the table owner) does not bypass RLS.
--
-- A tenant's customizations are a tenant's business (INV-T1). Unlike
-- object_schemas — whose core-layer rows are deliberately readable by every
-- tenant, because a shipped object is shared — there is no cross-tenant row
-- here: every layer this table holds is somebody's.
ALTER TABLE object_overlays ENABLE ROW LEVEL SECURITY;
ALTER TABLE object_overlays FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_object_overlays ON object_overlays
	USING (tenant_id = current_setting('app.tenant_id', true));

-- INV-T1: no query path returns another tenant's rows. current_setting's
-- missing_ok=true means an unset app.tenant_id yields NULL, which the
-- comparison never matches — no context, zero rows, rather than an error.
-- FORCE is required in addition to ENABLE: table owners (the role the
-- app connects as, since it also ran migrations) bypass RLS unless the
-- table is forced to apply policies to its owner too.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_users ON users
	USING (tenant_id = current_setting('app.tenant_id', true));

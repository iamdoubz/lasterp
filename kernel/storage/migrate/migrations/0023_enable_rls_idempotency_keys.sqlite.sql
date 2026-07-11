-- See 0016_enable_rls_object_schemas.sqlite.sql: RLS is Postgres-only; on
-- SQLite (solo mode) the repository layer filters by tenant_id itself.
SELECT 1;

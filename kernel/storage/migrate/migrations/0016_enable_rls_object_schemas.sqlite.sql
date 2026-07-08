-- RLS is Postgres-only (ADR-005); SQLite (solo mode) relies on
-- kernel/metadata always filtering by tenant_id (or layer = 'core') instead.
SELECT 1;

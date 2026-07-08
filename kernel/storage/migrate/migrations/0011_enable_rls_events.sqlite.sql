-- RLS is Postgres-only (ADR-005); SQLite (solo mode) relies on
-- kernel/eventstore always filtering by tenant_id instead.
SELECT 1;

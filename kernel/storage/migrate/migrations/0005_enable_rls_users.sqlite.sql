-- RLS is a Postgres-only mechanism; SQLite (solo mode) bypasses it per
-- ADR-005 and relies on the repository layer always filtering by
-- tenant_id instead. No-op kept as its own version so schema_migrations
-- stays aligned across dialects.
SELECT 1;

-- SQLite has no RLS engine; tenant isolation on the single-tenant replica is
-- the repository's tenant_id predicate (ADR-005). No-op migration to keep the
-- per-dialect migration set aligned.
SELECT 1;

-- SQLite has no RLS engine; isolation on the single-tenant replica is the
-- tenant_id predicate every recovery-code query applies (ADR-005). No-op.
SELECT 1;

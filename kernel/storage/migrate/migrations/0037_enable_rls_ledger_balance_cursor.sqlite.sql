-- SQLite has no RLS engine; isolation on the single-tenant replica is the
-- tenant_id predicate the projection queries apply (ADR-005). No-op.
SELECT 1;

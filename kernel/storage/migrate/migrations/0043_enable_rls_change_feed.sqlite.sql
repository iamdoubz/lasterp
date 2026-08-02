-- SQLite has no RLS engine; isolation is the tenant_id predicate every feed
-- query applies (ADR-005). No-op.
SELECT 1;

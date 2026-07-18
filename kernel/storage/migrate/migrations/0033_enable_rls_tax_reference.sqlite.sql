-- SQLite has no RLS engine; tenant isolation on the single-tenant replica is the
-- repository's tenant_id predicate (ADR-005), which modules/tax applies
-- explicitly in every lookup. No-op migration to keep the per-dialect set
-- aligned.
SELECT 1;

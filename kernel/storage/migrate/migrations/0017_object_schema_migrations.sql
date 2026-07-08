-- Tracks which generated DDL (kernel/metadata.ApplyDDL) has been applied,
-- separate from schema_migrations (kernel/storage/migrate), which is
-- scoped to kernel tables fixed at compile time — see
-- docs/notes/WP-0.5-decisions.md, decision 4.
--
-- Not tenant-scoped, no RLS: the physical table a generated object's DDL
-- creates is shared across every tenant (that's the point of the
-- tenant_id column + RLS policy GenerateDDL adds to it) — applying its
-- DDL is a one-time global operation, like schema_migrations itself, not
-- a per-tenant one.
CREATE TABLE object_schema_migrations (
	object TEXT NOT NULL,
	version INT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (object, version)
);

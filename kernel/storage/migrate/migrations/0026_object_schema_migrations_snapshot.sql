-- WP-1.0a metadata DDL evolution: record the effective schema applied at
-- each version so ApplyDDL can diff the next version against the last one
-- (the "schema history store" WP-0.5 decision 3 said a real diff would need).
-- Nullable: rows written before this column existed (there are none pre-1.0,
-- but the create path also tolerates a legacy NULL baseline by falling back
-- to a CREATE — see kernel/metadata.ApplyDDL). Both dialects support
-- ADD COLUMN of a nullable TEXT.
ALTER TABLE object_schema_migrations ADD COLUMN applied_schema TEXT;

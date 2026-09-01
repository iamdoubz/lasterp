-- WP-3.2c: tenant metadata overlays — ADR-006's customization layer, persisted.
--
-- Until this table existed, `metadata.Merge` was only ever called with zero
-- overlays: the "customization is data, not forks" promise had core objects and
-- nothing to stack on them. A row here is one customization layer for one
-- object in one tenant.
--
-- Separate from `object_schemas` rather than a column on it (decisions §3): that
-- table holds whole *Object* definitions keyed (tenant, name, layer, version),
-- and an overlay is a different document with a different key — a tenant may
-- hold several plugin overlays on one object, and uninstall has to remove
-- exactly one plugin's.
--
-- No DDL follows from a row here. Overlay-added fields live in the generated
-- table's fixed `custom_fields` blob (kernel/metadata/ddl.go, ADR-006), because
-- the physical table is shared by every tenant — which is precisely why adding a
-- field for one tenant alters nothing (decisions §1).
CREATE TABLE object_overlays (
	tenant_id TEXT NOT NULL,
	-- The shipped object this overlay targets. Not `object`: reserved in
	-- SQL:2011, and a column name that needs quoting in one dialect is a
	-- portability trap the storage adapter should not have to carry.
	object_name TEXT NOT NULL,
	-- The ADR-006 layer: 'plugin' or 'tenant'. 'core' and 'module' are shipped
	-- definitions and belong in object_schemas, not here.
	layer TEXT NOT NULL,
	-- Who owns this layer: the plugin id, or '' for the tenant's own overlay.
	-- Part of the key so uninstalling one plugin cannot take another's with it.
	source TEXT NOT NULL DEFAULT '',
	-- The YAML document as written, verbatim — the same call plugins.manifest
	-- and automations.definition make. The stored record is what an
	-- administrator approved, not this host's re-marshalling of it, and it is
	-- what ADR-006's exportable "customization packages" are made of.
	definition TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	-- INV-T4: a schema change is a mutation, and it names who made it.
	updated_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, object_name, layer, source)
);

-- The request-path read: every overlay for one object in one tenant
-- (kernel/metadata.Resolve). tenant_id first, as every index in this schema is
-- (ADR-005).
CREATE INDEX idx_object_overlays_object ON object_overlays (tenant_id, object_name);

-- The uninstall read: every overlay one plugin owns, across objects.
CREATE INDEX idx_object_overlays_source ON object_overlays (tenant_id, source);

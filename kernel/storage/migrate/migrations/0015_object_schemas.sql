-- The metadata engine's schema registry (docs/03-DATA-MODEL.md, ADR-006).
-- One row per layer per object per tenant. Core-layer definitions (shipped
-- by the kernel/modules, not tenant-specific) use tenant_id = '' as the
-- sentinel — every other layer (module/plugin/tenant) is a real tenant_id.
CREATE TABLE object_schemas (
	tenant_id TEXT NOT NULL,
	name TEXT NOT NULL,
	layer TEXT NOT NULL,
	version INT NOT NULL,
	definition TEXT NOT NULL,
	checksum TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, name, layer, version)
);

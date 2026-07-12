-- Per-tenant module enable-state (ADR-018 §5: disable != delete). Enabling a
-- module writes rows for it and its dependency closure; disabling flips
-- enabled to false but never removes the row or any module data. tenant_id is
-- the first column of the primary key (tenancy commandment); RLS is added in
-- 0025.
CREATE TABLE module_state (
	tenant_id TEXT NOT NULL,
	module TEXT NOT NULL,
	enabled BOOLEAN NOT NULL,
	mode TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL,
	updated_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, module)
);

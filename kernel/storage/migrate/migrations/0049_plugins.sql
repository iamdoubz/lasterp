-- Installed WASM plugins (WP-3.1a, ADR-007, docs/05).
--
-- `granted` is the authority an administrator actually approved, stored beside
-- the manifest that requested it so the two can be compared later: the manifest
-- is what the author asked for, `granted` is what this tenant agreed to. The
-- role named by role_id is where that authority actually lives — this column is
-- the human-readable record of the approval, not the enforcement.
--
-- `module` is the WASM binary, base64 in TEXT: the two dialects disagree on
-- byte columns and the adapter has no precedent (same reasoning as the secrets
-- vault, WP-3.0-decisions.md §10). Bytes live in the database rather than on
-- disk so a multi-node deployment needs no shared filesystem.
--
-- tenant_id is the first column of the primary key (tenancy commandment); RLS
-- is added in 0050.
CREATE TABLE plugins (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	version TEXT NOT NULL,
	manifest TEXT NOT NULL,
	granted TEXT NOT NULL,
	role_id TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	module TEXT NOT NULL,
	installed_at TIMESTAMPTZ NOT NULL,
	installed_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

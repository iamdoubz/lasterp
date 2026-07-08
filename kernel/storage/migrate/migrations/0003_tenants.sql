-- Global root table: tenants themselves are not tenant-scoped (ADR-005).
CREATE TABLE tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);

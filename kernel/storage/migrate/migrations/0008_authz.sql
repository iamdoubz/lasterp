CREATE TABLE roles (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL REFERENCES tenants(id),
	name TEXT NOT NULL,
	is_core BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE UNIQUE INDEX idx_roles_tenant_name ON roles (tenant_id, name);

-- condition is stored for forward compatibility with the CEL-based
-- conditions described in docs/08-SECURITY-MULTITENANCY.md, but WP-0.3's
-- evaluator only supports NULL (unconditional grant) — see
-- docs/notes/WP-0.3-decisions.md.
CREATE TABLE role_permissions (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	role_id TEXT NOT NULL REFERENCES roles(id),
	object TEXT NOT NULL,
	action TEXT NOT NULL,
	condition TEXT
);

CREATE UNIQUE INDEX idx_role_permissions_unique ON role_permissions (role_id, object, action);

CREATE TABLE user_roles (
	tenant_id TEXT NOT NULL,
	user_id TEXT NOT NULL,
	role_id TEXT NOT NULL REFERENCES roles(id),
	PRIMARY KEY (user_id, role_id)
);

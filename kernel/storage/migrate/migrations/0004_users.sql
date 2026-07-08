CREATE TABLE users (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL REFERENCES tenants(id),
	email TEXT NOT NULL,
	password_hash TEXT,
	totp_secret TEXT,
	totp_enabled BOOLEAN NOT NULL DEFAULT FALSE,
	totp_last_counter BIGINT,
	created_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX idx_users_tenant_email ON users (tenant_id, email);

-- Composite unique index backing sessions' (tenant_id, user_id) FK below.
CREATE UNIQUE INDEX idx_users_tenant_id ON users (tenant_id, id);

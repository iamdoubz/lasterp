CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	user_id TEXT NOT NULL,
	device_id TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	refresh_token_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	revoked_at TIMESTAMPTZ,
	FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id)
);

CREATE UNIQUE INDEX idx_sessions_token_hash ON sessions (token_hash);
CREATE UNIQUE INDEX idx_sessions_refresh_token_hash ON sessions (refresh_token_hash);

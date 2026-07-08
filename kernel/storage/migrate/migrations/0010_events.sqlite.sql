-- See 0010_events.postgres.sql. SQLite has no BIGSERIAL/JSONB: INTEGER
-- PRIMARY KEY is still a monotonic autoincrementing rowid alias here
-- (never reused, since we only ever insert), and payload is stored as
-- TEXT (marshaled JSON) — modernc.org/sqlite has no native JSON type.
CREATE TABLE events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	stream_id TEXT NOT NULL,
	version INT NOT NULL,
	type TEXT NOT NULL,
	schema_version INT NOT NULL,
	payload TEXT NOT NULL,
	actor_id TEXT NOT NULL,
	command_id TEXT NOT NULL,
	occurred_at TIMESTAMPTZ NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX idx_events_tenant_stream_version ON events (tenant_id, stream_id, version);
CREATE UNIQUE INDEX idx_events_command_id ON events (command_id);

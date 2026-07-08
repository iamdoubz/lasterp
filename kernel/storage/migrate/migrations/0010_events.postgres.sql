-- Event store (financial truth; ADR-003). id is the global, gapless,
-- monotonic cursor the change feed reads from (docs/03-DATA-MODEL.md).
CREATE TABLE events (
	id BIGSERIAL PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	stream_id TEXT NOT NULL,
	version INT NOT NULL,
	type TEXT NOT NULL,
	schema_version INT NOT NULL,
	payload JSONB NOT NULL,
	actor_id TEXT NOT NULL,
	command_id TEXT NOT NULL,
	occurred_at TIMESTAMPTZ NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL
);

-- INV-E2: optimistic concurrency enforced by the database, not app logic —
-- a concurrent append at a stale version hits this constraint.
CREATE UNIQUE INDEX idx_events_tenant_stream_version ON events (tenant_id, stream_id, version);

-- INV-E4: command_id uniqueness makes replay/retry produce exactly-once
-- effects; kernel/eventstore.Append inspects a violation of this specific
-- index (by name/message) to distinguish "already applied, return it" from
-- a genuine version conflict.
CREATE UNIQUE INDEX idx_events_command_id ON events (command_id);

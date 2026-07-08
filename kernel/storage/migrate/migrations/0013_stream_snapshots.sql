-- One snapshot per stream (overwritten via upsert as newer ones are
-- taken); kernel/eventstore.LoadStream reads this plus events after its
-- version instead of always folding from event zero.
CREATE TABLE stream_snapshots (
	tenant_id TEXT NOT NULL,
	stream_id TEXT NOT NULL,
	version INT NOT NULL,
	state TEXT NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, stream_id)
);

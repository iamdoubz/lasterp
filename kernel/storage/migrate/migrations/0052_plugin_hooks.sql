-- WP-3.1b: hook delivery state.
--
-- Circuit-breaker counters live on the plugin row itself, which dispatch
-- already loads. Not in memory: state that resets on restart turns "repeated
-- failures trip a breaker" into "…until something restarts", and a plugin bad
-- enough to trip a breaker is exactly the one restarting the process
-- (WP-3.1b-decisions.md §4).
ALTER TABLE plugins ADD COLUMN hook_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plugins ADD COLUMN breaker_opened_at TIMESTAMPTZ;

-- One cursor per plugin into the tenant's change feed. The async runner is a
-- feed consumer (§5): changefeed.Read already gives ordered, resumable,
-- exactly-once-observed delivery under INV-S5, so there is no queue to invent.
CREATE TABLE plugin_deliveries (
	tenant_id TEXT NOT NULL,
	plugin_id TEXT NOT NULL,
	cursor BIGINT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, plugin_id)
);

-- Deliveries that failed every retry. Nothing is dropped silently: the same
-- promise INV-S4 makes for rejected offline commands, for the same reason —
-- work that vanished is worse than work that failed loudly.
CREATE TABLE plugin_dead_letters (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	plugin_id TEXT NOT NULL,
	fn TEXT NOT NULL,
	cursor BIGINT NOT NULL,
	object TEXT NOT NULL,
	ref_id TEXT NOT NULL,
	error TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	failed_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_plugin_dead_letters_plugin ON plugin_dead_letters (tenant_id, plugin_id, failed_at);

-- Plugin-scoped key/value storage (docs/05's `kv.get/set`). It ships in this WP
-- rather than a later one because at-least-once delivery makes idempotency the
-- hook author's job, and a dedupe key needs somewhere to live.
CREATE TABLE plugin_kv (
	tenant_id TEXT NOT NULL,
	plugin_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, plugin_id, key)
);

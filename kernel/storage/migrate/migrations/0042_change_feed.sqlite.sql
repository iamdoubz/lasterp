-- See 0042_change_feed.postgres.sql. INTEGER PRIMARY KEY AUTOINCREMENT is the
-- monotonic rowid alias, as on events.
--
-- The commit-order hazard the Postgres file describes does not arise here:
-- SQLite takes a write lock for the duration of a write transaction, so only
-- one writer holds an unassigned id at a time and id order IS commit order.
-- The reader's horizon is therefore unbounded on this dialect — one adapter
-- method, not a second reader (decisions §2).
CREATE TABLE change_feed (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id   TEXT NOT NULL,
	source      TEXT NOT NULL,
	ref_id      TEXT NOT NULL,
	object      TEXT NOT NULL,
	scope_key   TEXT NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_change_feed_tenant_id ON change_feed (tenant_id, id);
CREATE INDEX idx_change_feed_tenant_scope ON change_feed (tenant_id, scope_key, id);

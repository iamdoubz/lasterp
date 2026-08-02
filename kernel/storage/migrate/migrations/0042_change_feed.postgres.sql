-- WP-2.1: the change feed — one totally-ordered per-tenant log over every
-- source a replica must follow (docs/04 §Concepts: "event store entries + CRUD
-- audit entries + metadata changes ... Global position = bigint cursor").
--
-- It holds POINTERS, not payloads. source + ref_id identify the row in events
-- or audit_log and readers hydrate from there. Copying event payloads in here
-- would put a second copy of financial truth where docs/19 allows none: two
-- copies can disagree, and the disagreement would be invisible until a replica
-- diverged. The cost is one extra query per batch, which is bounded.
--
-- Why a table at all, rather than ordering a UNION of events and audit_log:
-- events.id is BIGSERIAL and audit_log.id is TEXT, so there is no shared order
-- to read, and ordering by recorded_at is not stable — timestamps tie, and a
-- resumed reader would see a different sequence than the first pass. That
-- breaks the resume AC outright. See WP-2.1-decisions.md §3.
--
-- id is BIGSERIAL and is the cursor, but note what it is NOT: it is not commit
-- order. A transaction can take id 5 and commit after one holding id 6, so a
-- reader that trusts "id > cursor" will step over 5 forever. The reader's
-- stable-horizon filter is what makes this column safe to page on (INV-S5,
-- decisions §2) — the column alone does not carry the guarantee.
CREATE TABLE change_feed (
	id          BIGSERIAL PRIMARY KEY,
	tenant_id   TEXT NOT NULL,
	source      TEXT NOT NULL,
	ref_id      TEXT NOT NULL,
	object      TEXT NOT NULL,
	scope_key   TEXT NOT NULL,
	recorded_at TIMESTAMPTZ NOT NULL
);

-- tenant_id first, per CLAUDE.md. Every read is "this tenant, after this
-- cursor", which is exactly this index.
CREATE INDEX idx_change_feed_tenant_id ON change_feed (tenant_id, id);

-- scope_key is written by every append but computed trivially until WP-2.4
-- owns scope computation (decisions §4). The index exists now so 2.4 replaces
-- a function body rather than migrating a populated table.
CREATE INDEX idx_change_feed_tenant_scope ON change_feed (tenant_id, scope_key, id);

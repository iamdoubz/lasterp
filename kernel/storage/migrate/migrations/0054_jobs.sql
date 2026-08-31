-- WP-3.3b: the durable job queue.
--
-- A table, not a broker. Solo mode is one binary (ADR-011), and the queue is
-- claimed by an atomic compare-and-set `UPDATE` rather than
-- `SELECT … FOR UPDATE SKIP LOCKED` — SQLite has no such clause, and forking
-- the claim per dialect would put the one piece of concurrency control that
-- matters on two code paths (docs/notes/WP-3.3-decisions.md §6).
CREATE TABLE jobs (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	kind TEXT NOT NULL,
	payload TEXT NOT NULL,
	-- dedupe_key is how an at-least-once producer stays idempotent: the
	-- plugin host's enqueue_job is called from an async hook that may be
	-- delivered twice (WP-3.1b), so the second enqueue must find the first.
	-- NULLs are distinct in a unique index on both dialects, so a job with no
	-- key is simply never deduplicated.
	dedupe_key TEXT,
	status TEXT NOT NULL,
	-- Microseconds since the Unix epoch, not TIMESTAMPTZ, and this is the one
	-- table in the tree that does it. Everywhere else a timestamp is read out
	-- and compared in Go (kernel/identity's expires_at, say); here the claim
	-- must be a single atomic statement, so the comparison has to happen inside
	-- SQL. On SQLite a TIMESTAMPTZ round-trips as a Go-formatted string with
	-- variable-length fractional seconds, which makes `<=` a lexicographic
	-- compare that is wrong for exactly the values that matter. An integer is
	-- ordered identically on both dialects. created_at/updated_at stay
	-- TIMESTAMPTZ: they are read by people, never compared by the queue.
	run_at_us BIGINT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	-- A lease, not a lock: a worker that dies mid-job leaves locked_until in
	-- the past and the job becomes claimable again. Nothing has to notice the
	-- crash.
	locked_until_us BIGINT,
	locked_by TEXT,
	-- claim_token is unique per Claim call, so the claimed row can be read back
	-- by the token rather than by id: between the UPDATE and the read another
	-- worker may have claimed a different row, and an id lookup would not say so.
	claim_token TEXT,
	last_error TEXT,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_jobs_due ON jobs (tenant_id, status, run_at_us);
CREATE UNIQUE INDEX idx_jobs_dedupe ON jobs (tenant_id, kind, dedupe_key);

-- Jobs that failed every attempt. Same promise, and the same reason, as
-- plugin_dead_letters (WP-3.1b) and INV-S4's rejected commands: a queue head
-- that retries forever blocks everything behind it, and work that vanished is
-- worse than work that failed loudly.
CREATE TABLE job_dead_letters (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	job_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	payload TEXT NOT NULL,
	error TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	failed_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_job_dead_letters_kind ON job_dead_letters (tenant_id, kind, failed_at);

-- Recurring work: one row per cron expression, carrying the next firing.
--
-- next_run_at is stored rather than recomputed from "now" on every pass,
-- because those two differ exactly when it matters: a deployment that was down
-- over a firing must decide whether that firing is owed, and a stored
-- next_run_at makes the answer visible instead of implicit in a clock reading.
CREATE TABLE job_schedules (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	kind TEXT NOT NULL,
	cron TEXT NOT NULL,
	payload TEXT NOT NULL,
	-- owner scopes a bulk delete: uninstalling a plugin removes its schedules
	-- without a LIKE over ids, the same way DeleteRole removes its authority.
	owner TEXT NOT NULL,
	next_run_at_us BIGINT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_job_schedules_due ON job_schedules (tenant_id, next_run_at_us);
CREATE INDEX idx_job_schedules_owner ON job_schedules (tenant_id, owner);

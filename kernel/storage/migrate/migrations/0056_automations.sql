-- WP-3.3b: automations — trigger → condition → action, stored as data.
--
-- A kernel table rather than a metadata object, for the reason `secrets` and
-- `plugins` are: an automation is configuration *of* the system, not a business
-- record in it, and it must stay out of the change feed and the replica —
-- otherwise an automation reacting to changes would react to edits of itself.
-- ADR-006's "customization is data, not forks" is satisfied by the definition
-- being a YAML document a tenant can export, version and re-import, which is
-- what `definition` holds verbatim.
CREATE TABLE automations (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	-- The document the administrator wrote, stored as written. The parsed
	-- columns beside it are a cache for querying; this is the record of intent,
	-- the same call plugins.manifest makes.
	definition TEXT NOT NULL,
	-- "object" (driven from the change feed) or "schedule" (driven from cron).
	trigger_kind TEXT NOT NULL,
	-- For an object trigger: the object name. Empty for a schedule trigger.
	trigger_object TEXT NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	created_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_automations_trigger ON automations (tenant_id, trigger_kind, trigger_object);

-- One cursor per automation into the tenant's change feed, exactly as
-- plugin_deliveries is (WP-3.1b): changefeed.Read is already ordered, resumable
-- and exactly-once-observed under INV-S5, so there is no queue to invent. The
-- cursor is written at creation rather than lazily on the first pass, because a
-- cursor created on first pass silently skips everything in between — the bug
-- WP-3.1b found and this table inherits the fix for.
CREATE TABLE automation_cursors (
	tenant_id TEXT NOT NULL,
	automation_id TEXT NOT NULL,
	cursor BIGINT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, automation_id)
);

-- What an automation did, and to what. Not the audit log: audit_log records the
-- write an action performed, attributed to the automation's principal, which is
-- the INV-T4 record. This is the operator's view of the *automation* — it fired,
-- the condition said yes or no, the action succeeded or failed — which is the
-- question "why did this invoice change" needs answered from the other side.
CREATE TABLE automation_runs (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	automation_id TEXT NOT NULL,
	-- The feed entry or schedule firing that caused this run.
	trigger_ref TEXT NOT NULL,
	-- "matched", "skipped" (condition false) or "failed".
	outcome TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	ran_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_automation_runs_automation ON automation_runs (tenant_id, automation_id, ran_at);

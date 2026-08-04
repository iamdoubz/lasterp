-- WP-2.5: devices — the thing a replica lives on, made into a row so it can be
-- named, revoked and wiped (docs/08 §Data protection, ADR-021).
--
-- sessions.device_id has existed since WP-0.3 as a free string the client
-- supplies, and RefreshSession already binds refresh to it. What was missing is
-- that a device was not an entity: there was nothing to list in an admin UI and
-- nothing to mark when an employee reports a stolen laptop.
--
-- Registration is implicit — IssueSession upserts here — rather than a separate
-- enrolment call. Any enrolment step a client could skip is one that leaves an
-- untracked replica, which is the exact thing this table exists to control
-- (WP-2.5-decisions.md §1).
--
-- No FOREIGN KEY from sessions.device_id: that column predates this table and
-- holds arbitrary strings from every existing session and fixture. The upsert
-- makes the relationship true going forward without a backfill that would have
-- to invent rows for sessions whose devices are long gone.
CREATE TABLE devices (
	tenant_id TEXT NOT NULL,
	id        TEXT NOT NULL,
	user_id   TEXT NOT NULL,
	label     TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL,
	last_seen_at TIMESTAMPTZ NOT NULL,

	-- revoked_at stops the device getting new sessions. wipe_requested_at also
	-- destroys what it already holds.
	revoked_at TIMESTAMPTZ,
	wipe_requested_at TIMESTAMPTZ,

	-- Delivered, not confirmed. Stamped the first time the server refuses a
	-- request from this device, which proves the instruction was *delivered*.
	-- Nothing can prove a remote client deleted anything — the disk is not ours
	-- — and a column called wipe_confirmed_at would be a claim the system
	-- cannot support (decisions §4).
	wipe_delivered_at TIMESTAMPTZ,

	PRIMARY KEY (tenant_id, id),
	FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id)
);

-- tenant_id first, per CLAUDE.md. The admin list is "this tenant's devices,
-- newest first"; the hot path is the PRIMARY KEY lookup joined from a session.
CREATE INDEX idx_devices_tenant_user ON devices (tenant_id, user_id, created_at);

-- WP-3.3c: outbound destinations — the generalised allowlist a non-plugin
-- principal can own.
--
-- A plugin's allowlist is its manifest, approved once at install. An automation
-- has no manifest, so the thing an administrator approves is a row: *this
-- deployment may call this host, and here is where its URL is kept*. An
-- automation's `webhook` action then names a destination id, never a URL, and
-- `Webhook:send` is what its creator must hold for it (INV-T3, see
-- docs/notes/WP-3.3c-decisions.md §1).
--
-- Registering one is `Webhook:manage`; using one is `Webhook:send`. They are
-- different powers on purpose — folded together, anyone who can write an
-- automation picks the host, which is the SSRF-and-exfiltration primitive the
-- WP exists to avoid.
CREATE TABLE outbound_destinations (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	-- The reviewable half, as `host` or `host:port`. This is what an
	-- administrator reads in a list and what the dialled address is checked
	-- against before the socket opens; the full URL is not here, because for a
	-- whole class of webhook APIs the path *is* the credential (WP-3.2b) and
	-- INV-K1 forbids persisting that in the clear.
	host TEXT NOT NULL,
	-- Names the kernel/secrets entry holding the full https:// URL. Read
	-- server-side at send time by a named reader, audited there (WP-3.0).
	secret_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	created_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, id)
);

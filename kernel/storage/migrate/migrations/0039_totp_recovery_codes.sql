-- WP-1.12: single-use recovery codes, the way back in when the authenticator
-- is gone.
--
-- Rows are marked used rather than deleted, so "3 of 10 remaining" is
-- derivable and a consumed code leaves a trace. Single-use is enforced by
-- UPDATE ... WHERE used_at IS NULL + RowsAffected == 1 (decisions §2), not by
-- a check-then-act in Go.
--
-- code_hash is SHA-256 hex of a 120-bit crypto/rand value, unsalted — the same
-- treatment kernel/identity/session.go gives bearer tokens, and for the same
-- reason: there is no precomputation to defeat against a 2^120 space, and a
-- deterministic hash keeps verification a single indexed lookup instead of
-- bcrypting a candidate against every row on an unauthenticated route.
CREATE TABLE totp_recovery_codes (
	tenant_id  TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	code_hash  TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	used_at    TIMESTAMPTZ,
	-- tenant_id first, per CLAUDE.md. The composite FK reuses
	-- idx_users_tenant_id exactly as sessions does.
	PRIMARY KEY (tenant_id, user_id, code_hash),
	FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id)
);

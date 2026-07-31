-- WP-1.9: bind a user to an external OIDC identity.
--
-- Two columns rather than a user_identities join table: one IdP per deployment
-- (WP-1.9-decisions.md §2) makes multiple identities per user unreachable, and
-- a join table would only add a join to every login.
ALTER TABLE users ADD COLUMN oidc_issuer TEXT;
ALTER TABLE users ADD COLUMN oidc_subject TEXT;

-- tenant_id first, per CLAUDE.md. NULLs compare as distinct in a unique index
-- on both Postgres and SQLite, so every password-only user coexists here
-- without a partial index (which the two dialects spell differently).
--
-- Uniqueness is the storage-layer half of "one external identity is one local
-- user": two users in a tenant cannot both claim the same IdP subject even if
-- the application layer is talked into trying.
CREATE UNIQUE INDEX idx_users_tenant_oidc ON users (tenant_id, oidc_issuer, oidc_subject);

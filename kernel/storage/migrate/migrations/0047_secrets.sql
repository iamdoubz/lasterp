-- The secrets vault (WP-3.0, docs/08 §Data protection): connector and plugin
-- credentials at rest, sealed with envelope encryption.
--
-- No column here holds plaintext. `ciphertext` is the value under a per-secret
-- data key; `wrapped_dek` is that data key under the deployment key named by
-- `key_id`. Rotation re-wraps `wrapped_dek` and leaves `ciphertext` alone —
-- which is why the data key is per row rather than per tenant (INV-K1,
-- WP-3.0-decisions.md §1).
--
-- Both are base64 in TEXT rather than BYTEA/BLOB: the two dialects disagree on
-- byte columns and the adapter has no precedent to follow, while these values
-- are tens of bytes (§10).
--
-- tenant_id is the first column of the primary key (tenancy commandment); RLS
-- is added in 0048.
CREATE TABLE secrets (
	tenant_id TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	key_id TEXT NOT NULL,
	wrapped_dek TEXT NOT NULL,
	ciphertext TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	updated_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, name)
);

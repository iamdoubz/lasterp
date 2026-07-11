-- Idempotency keys for the API gateway (ADR-009, WP-0.6): every write
-- carries an Idempotency-Key; a replay of the same (tenant, key) returns the
-- stored response instead of re-executing. request_fingerprint (SHA-256 of
-- method+path+body) detects a key reused for a different request.
-- response_status = 0 marks a reserved-but-not-yet-finalized (in-flight)
-- request. tenant_id first, per the tenancy rule.
CREATE TABLE idempotency_keys (
	tenant_id TEXT NOT NULL,
	idem_key TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	response_status INT NOT NULL,
	response_body TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, idem_key)
);

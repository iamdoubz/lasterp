-- Kernel-enforced audit log for CRUD-domain objects (ADR-003: "CRUD
-- domains: master data ... as ordinary rows with a mandatory,
-- kernel-enforced audit log"). One of the three immutable trails
-- (08-SECURITY-MULTITENANCY.md), alongside the event store and agent_audit.
CREATE TABLE audit_log (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	object TEXT NOT NULL,
	record_id TEXT NOT NULL,
	action TEXT NOT NULL,
	changes TEXT NOT NULL,
	actor_id TEXT NOT NULL,
	at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_audit_log_tenant_object_record ON audit_log (tenant_id, object, record_id);

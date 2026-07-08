-- See kernel/eventstore's 0012_events_append_only_trigger.postgres.sql:
-- same defensive pattern, applied to the audit trail (INV-T4 for
-- CRUD-domain writes — docs/notes/WP-0.5-decisions.md, decision 6).
CREATE FUNCTION reject_audit_log_mutation() RETURNS TRIGGER AS $$
BEGIN
	RAISE EXCEPTION 'audit_log is append-only: % not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update
	BEFORE UPDATE ON audit_log
	FOR EACH ROW EXECUTE FUNCTION reject_audit_log_mutation();

CREATE TRIGGER audit_log_no_delete
	BEFORE DELETE ON audit_log
	FOR EACH ROW EXECUTE FUNCTION reject_audit_log_mutation();

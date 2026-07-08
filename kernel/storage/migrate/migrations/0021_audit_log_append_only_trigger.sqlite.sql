CREATE TRIGGER audit_log_no_update
	BEFORE UPDATE ON audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit_log is append-only: UPDATE not permitted');
END;

CREATE TRIGGER audit_log_no_delete
	BEFORE DELETE ON audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit_log is append-only: DELETE not permitted');
END;

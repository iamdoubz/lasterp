-- See 0012_events_append_only_trigger.postgres.sql (INV-E1, partial).
CREATE TRIGGER events_no_update
	BEFORE UPDATE ON events
BEGIN
	SELECT RAISE(ABORT, 'events is append-only: UPDATE not permitted');
END;

CREATE TRIGGER events_no_delete
	BEFORE DELETE ON events
BEGIN
	SELECT RAISE(ABORT, 'events is append-only: DELETE not permitted');
END;

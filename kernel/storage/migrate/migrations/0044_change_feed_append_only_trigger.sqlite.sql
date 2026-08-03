CREATE TRIGGER change_feed_no_update
	BEFORE UPDATE ON change_feed
BEGIN
	SELECT RAISE(ABORT, 'change_feed is append-only: UPDATE not permitted');
END;

CREATE TRIGGER change_feed_no_delete
	BEFORE DELETE ON change_feed
BEGIN
	SELECT RAISE(ABORT, 'change_feed is append-only: DELETE not permitted');
END;

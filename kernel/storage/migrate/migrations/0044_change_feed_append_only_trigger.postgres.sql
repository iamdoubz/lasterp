-- Same defensive pattern as events (0012) and audit_log (0021), applied to the
-- change feed.
--
-- A feed a writer can go back and edit is a feed that can lie to a replica
-- that has already consumed it: the client has no way to learn that cursor 7
-- now says something different from what it applied. INV-S5's "observed
-- exactly once, in a stable total order" is only meaningful if entries are
-- immutable once written, so the feed joins the append-only tables rather than
-- relying on nobody writing the UPDATE.
CREATE FUNCTION reject_change_feed_mutation() RETURNS TRIGGER AS $$
BEGIN
	RAISE EXCEPTION 'change_feed is append-only: % not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER change_feed_no_update
	BEFORE UPDATE ON change_feed
	FOR EACH ROW EXECUTE FUNCTION reject_change_feed_mutation();

CREATE TRIGGER change_feed_no_delete
	BEFORE DELETE ON change_feed
	FOR EACH ROW EXECUTE FUNCTION reject_change_feed_mutation();

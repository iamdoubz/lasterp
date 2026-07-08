-- INV-E1 (partial): reject any UPDATE/DELETE on events outright. This is
-- a defensive trigger any connection hits, including the migrations/app
-- role itself — it is not a substitute for WP-0.8's DB role separation
-- (a role with no UPDATE/DELETE grant at all on this table), which closes
-- the remaining gap ("impossible", not just "forbidden") — see
-- docs/notes/WP-0.4-decisions.md, decision 1.
CREATE FUNCTION reject_events_mutation() RETURNS TRIGGER AS $$
BEGIN
	RAISE EXCEPTION 'events is append-only: % not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_no_update
	BEFORE UPDATE ON events
	FOR EACH ROW EXECUTE FUNCTION reject_events_mutation();

CREATE TRIGGER events_no_delete
	BEFORE DELETE ON events
	FOR EACH ROW EXECUTE FUNCTION reject_events_mutation();

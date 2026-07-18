-- WP-1.2 PR-B: the docs/19 layer-3 pipeline-owned write paths for the event
-- log. After EnforceLedgerPipelineGrants revokes INSERT on events from the app
-- role, these SECURITY DEFINER functions are the ONLY way the app role can
-- append to events — so a raw INSERT from the app role fails (INV-F5), and the
-- balance/open-period checks in ledger_post_entry are enforced by the database,
-- not merely by application code (INV-F1/F3 at the storage layer).
--
-- Both functions derive tenant_id from the session GUC (app.tenant_id, set by
-- kernel/tenancy.WithTenant) rather than a caller argument, so a caller cannot
-- forge another tenant's events even though SECURITY DEFINER may bypass RLS.
-- SET LOCAL check_function_bodies = off: ledger_post_entry references obj_period,
-- a metadata-generated table created at runtime (ledger.Register), not by a
-- migration — defer the body's table-existence check to call time.
SET LOCAL check_function_bodies = off;

-- append_event is the generic pipeline INSERT: kernel/eventstore.Append routes
-- its Postgres write through here. It performs no domain validation — the
-- version/command_id uniqueness that Append relies on stays enforced by the
-- unique indexes, which fire inside this INSERT exactly as before.
CREATE FUNCTION append_event(
	p_stream_id TEXT, p_version INT, p_type TEXT, p_schema_version INT,
	p_payload TEXT, p_actor_id TEXT, p_command_id TEXT,
	p_occurred_at TIMESTAMPTZ, p_recorded_at TIMESTAMPTZ
) RETURNS BIGINT
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
	v_tenant TEXT := current_setting('app.tenant_id', true);
	v_id BIGINT;
BEGIN
	IF v_tenant IS NULL OR v_tenant = '' THEN
		RAISE EXCEPTION 'append_event: no tenant context set';
	END IF;
	INSERT INTO events (tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
	VALUES (v_tenant, p_stream_id, p_version, p_type, p_schema_version, p_payload::jsonb, p_actor_id, p_command_id, p_occurred_at, p_recorded_at)
	RETURNING id INTO v_id;
	RETURN v_id;
END;
$$;

-- ledger_post_entry is the ledger's posting path: it re-checks the entry
-- balances (INV-F1) and its period is open (INV-F3) in SQL, then appends the
-- posted event — atomically, closing the check-then-append window the Go
-- pipeline alone leaves. It is idempotent on command_id (INV-E4): a replay
-- returns the original event.
CREATE FUNCTION ledger_post_entry(
	p_stream_id TEXT, p_period TEXT, p_payload TEXT, p_actor_id TEXT,
	p_command_id TEXT, p_occurred_at TIMESTAMPTZ, p_recorded_at TIMESTAMPTZ
) RETURNS TABLE(out_id BIGINT, out_stream TEXT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
	v_tenant TEXT := current_setting('app.tenant_id', true);
	v_debits BIGINT;
	v_credits BIGINT;
	v_status TEXT;
	v_existing_id BIGINT;
	v_existing_stream TEXT;
BEGIN
	IF v_tenant IS NULL OR v_tenant = '' THEN
		RAISE EXCEPTION 'ledger_post_entry: no tenant context set';
	END IF;

	-- INV-E4: a replayed command returns the original event, no double effect.
	SELECT id, stream_id INTO v_existing_id, v_existing_stream
		FROM events WHERE tenant_id = v_tenant AND command_id = p_command_id;
	IF FOUND THEN
		out_id := v_existing_id;
		out_stream := v_existing_stream;
		RETURN NEXT;
		RETURN;
	END IF;

	-- INV-F1: Σdebits = Σcredits, summed from the JSON lines.
	SELECT COALESCE(SUM((l->>'debit')::BIGINT), 0), COALESCE(SUM((l->>'credit')::BIGINT), 0)
		INTO v_debits, v_credits
		FROM jsonb_array_elements((p_payload::jsonb)->'lines') AS l;
	IF v_debits <> v_credits THEN
		RAISE EXCEPTION 'ledger: entry does not balance (debits % <> credits %)', v_debits, v_credits
			USING ERRCODE = 'check_violation';
	END IF;

	-- INV-F3: the period must exist and be open.
	SELECT status INTO v_status FROM obj_period
		WHERE tenant_id = v_tenant AND code = p_period AND archived_at IS NULL;
	IF NOT FOUND THEN
		RAISE EXCEPTION 'ledger: period % not found', p_period USING ERRCODE = 'check_violation';
	END IF;
	IF v_status <> 'open' THEN
		RAISE EXCEPTION 'ledger: period % is closed', p_period USING ERRCODE = 'check_violation';
	END IF;

	INSERT INTO events (tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
	VALUES (v_tenant, p_stream_id, 1, 'ledger.entry.posted', 1, p_payload::jsonb, p_actor_id, p_command_id, p_occurred_at, p_recorded_at)
	RETURNING id, stream_id INTO out_id, out_stream;
	RETURN NEXT;
END;
$$;

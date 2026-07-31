-- WP-1.6 PR-B: how far the ledger_balances projection has been caught up.
--
-- Before this, ledger_balances was only ever written by an explicit full
-- RebuildBalances call — and no product code path made one, so the projection
-- was empty in a running system while reports were about to depend on it.
--
-- The cursor records the last event id folded into the projection, so a read can
-- apply only what is new (EnsureBalances) instead of re-folding the whole log.
-- It is derived state like the balances themselves: deleting this row and the
-- balances is always safe, because a rebuild reconstructs both from the event
-- log (INV-E5).
CREATE TABLE ledger_balance_cursor (
	tenant_id TEXT PRIMARY KEY,
	last_event_id BIGINT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

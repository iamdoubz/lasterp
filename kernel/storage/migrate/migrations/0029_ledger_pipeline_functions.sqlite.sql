-- SQLite has no SECURITY DEFINER, no roles, and solo mode is a single trusted
-- process (ADR-005): the ledger's balance/open-period checks are enforced by
-- the Go posting pipeline (the storage owner in solo mode) and mutation is
-- blocked by the append-only trigger. No pipeline functions here — no-op to
-- keep the per-dialect migration set aligned.
SELECT 1;

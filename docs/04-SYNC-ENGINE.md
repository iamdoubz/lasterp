# 04 — Sync Engine

The Lotus Notes promise (work anywhere, replicate later) with a referee. Decision record: [ADR-004](adr/ADR-004-sync-model.md).

## Concepts

- **Sync scope:** a named, server-defined subset of data a device replicates. Computed from role + module filters + explicit selections (e.g., "my region's customers, open documents last 24 months, all reference data"). Scopes keep replicas small and enforce least-privilege offline.
- **Change feed:** totally-ordered per-tenant log = event store entries + CRUD audit entries + metadata changes, each tagged with scope keys. Global position = `bigint` cursor.
- **Replica:** client SQLite DB, schema generated from the same metadata as the server. Contains: replicated tables, `_outbox` (pending commands), `_sync_state` (cursors, scope versions), `_conflicts`.

## Downstream (server → client)

1. Client connects (gRPC-web stream / WebSocket), presents device token + cursor per scope.
2. Server streams changes since cursor, filtered by scope + row-level entitlements, in batches with backpressure. Client applies in order inside a transaction, advances cursor. Crash-safe: cursor advances only after commit.
3. Live mode: connected clients receive pushes within ~1s of commit.
4. **Scope change / entitlement revocation:** server bumps scope version → client performs partial re-shape (delete out-of-scope rows, fetch newly-in-scope). Revocation also queues a purge instruction; the client honors it on next connect (documented limitation: a stolen offline device retains its replica → device-level encryption + remote-wipe token, see 08-SECURITY).
5. Initial hydration: snapshot download (paged) at a consistent position, then stream from there.

## Upstream (client → server)

1. User acts offline → command written to `_outbox` (with client-generated `command_id` UUIDv7) → optimistically applied to replica, rows flagged `pending`.
2. On connect, outbox drains in order. Server runs each command through the identical pipeline as online requests: authn → tenant context → authorization → business validation against **current** state → append events / mutate + audit.
3. Per-command outcome:
   - **accepted** → client clears pending flag; server changes flow back via the normal feed (client dedupes by command_id).
   - **accepted-with-rebase** → command was commutative (e.g., new journal entry, new CRM note); applied on newer state; any server-assigned values (document numbers!) returned and patched into the replica.
   - **rejected {code, reason, server_state}** → client rolls back the optimistic rows, files a `_conflicts` entry; UI shows a "Needs attention" tray: view server state, edit & resubmit, or discard.
4. Dependent commands (draft → then post) form a causal chain; a rejection cascades rejection of dependents with one grouped conflict entry.

## Conflict policy by data class

| Data class | Policy |
|---|---|
| New event-sourced documents (invoice draft, journal, CRM activity) | Almost always accepted; append-only is naturally commutative |
| Workflow transitions (post, pay, approve) | Server revalidates preconditions (period open, credit limit, stream version); reject on violation |
| Master-data edits | Field-level merge when disjoint fields changed; conflict tray when same field changed; **no silent LWW** |
| Reference data (rates, tax tables) | Server-authoritative, read-only on clients |
| Prose fields (notes) | Phase 5+: Yjs CRDT merge; until then field-level rule above |

## Offline capability matrix (sane defaults, tenant-tunable)

Offline-allowed: create/edit drafts, record CRM data, record time/expenses, pick/pack/count inventory (movements post on sync), view everything in scope.
Online-required by default: posting to GL, payment execution, payroll approval, period close (these need current-state guarantees; tenants can relax per-action with eyes open).

## Implementation status (WP-2.1: the downstream feed)

Shipped in [`kernel/changefeed`](../kernel/changefeed/): the `change_feed` table (one
totally-ordered per-tenant log over the event store and the CRUD audit trail), appended in
the same transaction as the write it describes, and read by cursor via
`GET /api/v1/sync/changes`. Entries are **pointers** — `source` + `ref_id` into `events` or
`audit_log` — so payloads live in exactly one place; readers hydrate from the source.

The cursor's guarantee is **INV-S5**: no committed change is skipped, every entry is
observed exactly once, and resuming from any position reproduces the same order. A
`BIGSERIAL` alone does not give that — a transaction takes its id at INSERT, not at commit,
so a writer holding id 5 open lets a later writer take 6 and commit first, and a reader
trusting `id > cursor` strands 5 permanently. A per-tenant ordering lock makes id order
equal commit order; cost and upgrade path in
[WP-2.1-decisions.md](notes/WP-2.1-decisions.md) §2.

Not yet built: the streaming transport (steps 1–3 above are served by cursored polling plus
an in-process notifier), scope computation (WP-2.4 — entries carry a scope tag, computed
trivially), snapshot hydration (WP-2.2), and everything upstream (WP-2.3).

## Guarantees & limits
- Acknowledged writes: RPO 0 (command accepted ⇔ events durably committed).
- Offline writes: at-least-once delivery, exactly-once effect (command_id dedupe).
- Ordering: per-client causal order preserved; cross-client order = server acceptance order.
- Clock skew irrelevant: server assigns `recorded_at`; client `occurred_at` kept for forensics only.

## Test plan (non-negotiable, see CLAUDE.md)
Deterministic simulation harness: N virtual clients × scripted partitions/crashes/interleavings × property checks (no lost accepted write, no double-entry violation, replica converges to server projection). Runs in CI on every kernel/sync PR.

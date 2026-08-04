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

## Implementation status (WP-2.2: the replica)

The server half (**WP-2.2a**) materialises the feed's pointers into current row state
(`GET /api/v1/sync/changes?include=rows`, re-authorised per object kind), pages a snapshot for
hydration (`GET /api/v1/sync/snapshot`), and conveys archived rows flagged rather than
filtering them — a delete a replica is never told about is a divergence no amount of syncing
repairs.

The client half (**WP-2.2b**) lives in [`web/src/sync/`](../web/src/sync/): the replica's
tables are generated from `GET /api/v1/meta/objects` (never a hand-written mirror — ADR-006,
and the schema is per-tenant), hydration pages each object and keeps the cursor from its
*first* page, and the apply loop folds feed pages in order, exactly once, inside one
transaction with the cursor. Values are checked against their declared field type on arrival
(INV-T5) rather than coerced. Per [ADR-017](adr/ADR-017-sync-client-core.md) the core is
TypeScript and the replica runs in a dedicated worker over the SAH-pool OPFS VFS.

**INV-S3** is proven by `TestReplicaConvergesToProjection` — randomized operations against the
real server on both dialects, with the real core on `node:sqlite` — plus one browser pass over
OPFS. That proof is itself guarded: `TestConvergenceHarnessDetectsASkippedFeed` deletes feed
entries and requires convergence to *fail*, because an apply loop writing current server state
measured against current server state can otherwise pass by construction. Decisions:
[WP-2.2b-decisions.md](notes/WP-2.2b-decisions.md).

Not yet built here: the streaming transport (steps 1–3 above are served by cursored polling
plus an in-process notifier). Scope computation landed in WP-2.4, below.

## Implementation status (WP-2.3: the outbox)

The replica writes as of **WP-2.3b**, in [`web/src/sync/outbox.ts`](../web/src/sync/outbox.ts).
One sentence carries the design: **a command is a stored HTTP request, and the drain is a
replay of it**. `_outbox` holds `{method, path, body, command_id}`; draining means issuing that
exact request through the same `web/src/api/client.ts` the online UI uses, at the same route.

**There is no sync write endpoint, and that is the decision.** INV-S2 — "offline commands pass
the identical validation pipeline; no privileged sync side door" — stops being a property to
test and becomes one there is no way to express a violation of. Three things then fall out
rather than being built: exactly-once is the gateway's idempotency store, because the
`command_id` *is* the `Idempotency-Key` (INV-E4); accept-with-rebase is the response body
applied to the replica; and a rejection is already problem+json, so the tray renders what the
server said rather than a translation of it.

- **Optimistic apply** writes the row and queues the command in one transaction, with the row's
  pre-image kept for rollback — a rejected *update* has nothing else to recover from, since
  server state never changed. Rows carrying unsent changes are flagged in a `_pending` sidecar
  rather than a column, so the INV-S3 convergence oracle stays a direct equality.
- **Reconnect order is drain → hydrate → pull**, with WP-2.4's revocation purge after it (a
  purge that runs first deletes rows queued commands reference).
- **The server accepts a client-supplied UUIDv7 row id**, so the row a user sees offline *is*
  the row the server ends up with — no provisional id to rewrite, and no queued command left
  pointing at something that moved. Document numbers are untouched: INV-F6 still allocates them
  server-side at acceptance.
- **The drain stops at the first rejection.** No command runs whose predecessor may have been
  its precondition; dependent-command chains (§Upstream 4) need no graph to be safe, only to be
  faster.
- **A denied `navigator.storage.persist()`** warns before the first write and caps the outbox
  rather than refusing offline writes outright — the browser can evict the origin without
  warning, and unsent work is the one thing no re-fetch reconstructs.

Upstream properties **INV-S1** (nothing acknowledged is lost), **INV-S2** and **INV-S4** (every
command ends accepted or in the tray) are carried by the scenarios WP-2.3b added to the
simulation harness below, plus
[`internal/app/sync_outbox_integrity_test.go`](../internal/app/sync_outbox_integrity_test.go).
Decisions: [WP-2.3-decisions.md](notes/WP-2.3-decisions.md).

The shipped screens still write online-first: routing `ObjectForm` through the outbox needs the
replica to be the *read* path too, which is one seam away and is not this WP (decisions §13).

## Implementation status (WP-2.4: scopes)

A **scope** is the set of scope keys a principal may replicate, computed from their role
grants: the objects they hold `read` on, intersected with what is replicable (a CRUD surface
exists) and what the tenant has enabled. `GET /api/v1/sync/scope` returns it, and
`GET /api/v1/sync/changes` is filtered to it — **pointers included**.

That last word is the change. WP-2.1 conveyed every pointer under one `sync:read` grant and
bounded the disclosure by argument ("names and ids, not contents"); WP-2.2 §3 kept the
asymmetry on purpose. Both are superseded: `sync:read` is now the right to follow *your own*
scope, and the objection that suppressing pointers would make cursors non-contiguous is
answered by §7 below rather than lived with.

- **Re-shape is a diff, and it has no state.** `_hydration` already records what a replica
  holds, so the client compares the server's scope against it: what left is purged, what
  arrived gets a `_hydration` row and is filled by the next hydration. There is no scope
  version and no server-side purge queue — docs/04 §Downstream 4 describes both, and a client
  that fetches the list on every reconnect gets the same behaviour from one round trip it was
  making anyway ([decisions](notes/WP-2.4-decisions.md) §2, §3).
- **A purge takes the server's data and never the user's.** It deletes replicated rows and
  nothing from `_outbox`, `_conflicts` or `_pending`. The row is a copy of something the
  server still holds; the queued command is work no re-fetch reconstructs. The re-shape
  therefore runs **after the drain** (WP-2.3 §4's rule, honoured) and **before the hydrate**
  (its refinement — a newly entitled object has no `_hydration` row to fill until the
  re-shape has added one). Order: `open → drain → reshape → hydrate → pull`.
- **The tray survives a revocation.** `_conflicts` carries the command's own body and the
  server's problem+json, so work refused *because* access was withdrawn still renders after
  its rows are gone. That was already true and is now tested, because nothing else would
  notice a later change that made the tray read the replica instead.
- **A filtered page still advances the cursor.** A scoped page can be short or empty while the
  feed has more, so the handler reports the feed's high-water mark — read *before* the page,
  which is what makes it safe under the ordering lock — as the resume position, and the client
  takes the server's cursor when it is ahead and refuses it when it is behind an entry the
  same page delivered.

Row-level scopes ("my region's customers, open documents last 24 months") are **not** built:
they need `authz` condition evaluation, which does not exist, and they subdivide these keys
rather than replacing them when it does. Decisions: [WP-2.4-decisions.md](notes/WP-2.4-decisions.md).

## Implementation status (WP-2.5: devices and remote wipe)

A device is now an entity (`devices`), registered implicitly when a session is issued — an
enrolment call a client could skip is one that leaves an untracked replica. Administrators
list, revoke and wipe them over `/api/v1/devices`.

**The wipe rides the authenticator, not an endpoint.** docs/08 says "honored at connect", and
the cheapest reading of that is a route the client polls — which a client can simply not call.
Instead `identity.ValidateSession` resolves the device behind the session, so a wiped device is
refused on *every* authenticated path (**INV-D1**) and the reconnect cycle honors the wipe at
whichever request happens to come first. Consequences worth knowing:

- **It is the one 401 that says why** (`type: "device-wiped"`). Every other authentication
  failure is deliberately undifferentiated to avoid a token oracle; this one is not, because a
  client that cannot tell a wipe from an expiry signs the user out and *keeps the replica*.
- **A wipe does not revoke the device's sessions.** If it did, session validation would fail
  first and the client would get a bare 401 with nothing to act on. The device keeps a session
  that can do exactly one thing: learn it has been wiped.
- **A wipe destroys unsent work — the deliberate opposite of WP-2.4's scope purge.** A purge
  spares `_outbox` because the queued command is the user's own work; a wipe takes it, because
  the device is presumed to be in the wrong hands. Both rules carry a test naming the other.
- **Delivery is recorded, erasure is not claimed.** `wipe_delivered_at` is stamped when the
  server first refuses the device. Nothing can prove a remote client deleted anything.

Replica at-rest encryption is **not** part of this: [ADR-021](adr/ADR-021-replica-at-rest-encryption.md)
makes it a native-shell control (WP-4.8) and amends docs/08 to state what protects a browser
replica instead. Decisions: [WP-2.5-decisions.md](notes/WP-2.5-decisions.md).

## Guarantees & limits
- Acknowledged writes: RPO 0 (command accepted ⇔ events durably committed).
- Offline writes: at-least-once delivery, exactly-once effect (command_id dedupe).
- Ordering: per-client causal order preserved; cross-client order = server acceptance order.
- Clock skew irrelevant: server assigns `recorded_at`; client `occurred_at` kept for forensics only.

## Test plan (non-negotiable, see CLAUDE.md)
Deterministic simulation harness: N virtual clients × scripted partitions/crashes/interleavings × property checks (no lost accepted write, no double-entry violation, replica converges to server projection). Runs in CI on every kernel/sync PR.

**Built in WP-2.3a** as [`internal/app/sync_simulation_integrity_test.go`](../internal/app/sync_simulation_integrity_test.go): four virtual clients, each its own replica file and its own driver process, synced concurrently against one server on both dialects, under a rotating schedule of two faults —

- **partition** (`--fail-after`): the wire is cut mid-sync, leaving the client partway;
- **crash** (`--kill-in-apply`): the process dies *inside* an apply transaction, after rows are written and before the cursor moves. This is the only fault that cannot be injected through the transport, and it is what turns the core's crash-safety claim into something a test can refute — an exception unwinds cleanly, a killed tab does not.

Two things keep the green tick meaningful. `TestSimulationHarnessDetectsADivergentClient` runs one client of the four against a feed with entries deleted and requires the suite to *fail*. And **every scheduled fault is asserted to have fired**, via a marker the driver writes at the injection point: the first version of this harness injected ten crashes across six rounds on both dialects, fired none of them, and reported green. A fault-injection suite that silently stops injecting is indistinguishable from one that passes.

**WP-2.3b added the upstream half** to this same harness rather than building another. Every client now also writes offline between rounds, so the partitioned client of each round is one that queued work it could not send, and the crashed one dies with commands in flight. After the storm the fleet's delivered work is counted **exactly**: too few means a write was lost, too many means one was applied twice, and both fail INV-S1.

One crash window is isolated separately, because nothing else reaches it: the process dies after the server has committed and before the client's record of having sent it is durable (`--kill-after-command`). Recovery must re-send and be deduplicated by the gateway (INV-E4). That is the difference between RPO 0 and "usually fine".

The harness is also what found two defects that were not in this WP's code:

- a `SQLITE_BUSY` on session validation surfaced as **401** rather than a retry, because the gateway mapped every authenticator error to "authentication required" — and a 401 is the one status clients act on destructively (the web client signs out; the drain would have filed queued work as *rejected*). Session validation now retries busy like every transactional path already did, and an unresolvable one is a 503;
- the crash injection counted `INSERT INTO obj_…`, which an optimistic create also is, so a client scheduled to crash mid-apply died while *queueing* instead — rolling back work and firing the fault nowhere near the window it was aimed at.

# WP-2.3 decisions — outbox & command replay

Roadmap: *"WP-2.3 Outbox & command replay: optimistic apply, pending flags, replay pipeline,
accept/reject/rebase, conflict tray UI. AC: simulation harness (04-SYNC-ENGINE.md test plan)
green; no-silent-loss property."*
Design: [docs/04](../04-SYNC-ENGINE.md) · [ADR-004](../adr/ADR-004-sync-model.md) ·
[ADR-017](../adr/ADR-017-sync-client-core.md) · [ADR-009](../adr/ADR-009-api-design.md) ·
[docs/19](../19-DATA-INTEGRITY.md) · [WP-2.2b-decisions.md](WP-2.2b-decisions.md).

The roadmap entry hands this WP three things to settle rather than discover (§5, §7, §10 of
WP-2.2b-decisions). They are §4, §5 and §6 below.

---

## 0. Split into WP-2.3a and WP-2.3b

The roadmap line says it itself: *"this WP is materially larger than its one roadmap line
suggests: the simulation harness is infrastructure gating its own AC, and is worth landing
separately against the existing read-only replica first."* Same precedent as WP-1.6 and WP-2.2.

- **WP-2.3a — the simulation harness.** docs/04 §Test plan asks for "N virtual clients ×
  scripted partitions/crashes/interleavings × property checks". Today
  `TestReplicaConvergesToProjection` is one client, twelve rounds, no faults. 2.3a grows *that
  test* into the harness rather than building a second one, and proves the downstream
  properties (INV-S3/S5) hold under concurrency and interruption. No new client code.
- **WP-2.3b — the outbox.** `_outbox`, optimistic apply, the drain, the conflict tray,
  INV-S1/S2/S4. Its AC becomes "add upstream scenarios to the harness", not "also build one".

Splitting is ordering, not extra work: 2.3b's acceptance criterion is *the harness green*, so
a single PR would review the harness and the thing it judges in the same diff.

## 1. A command is a stored HTTP request, and the drain is a replay of it

The outbox row holds `{method, path, body, command_id}`. Draining it means issuing that exact
request through the same `web/src/api/client.ts` the online UI uses, against the same route.

**There is no sync replay endpoint, and that is the decision.** INV-S2 — "offline commands pass
the identical validation pipeline as online writes; no privileged sync side door" — stops being
a property to test and becomes one there is no way to express a violation of. A batch endpoint
that unpacked commands and dispatched them internally would be a second write path with its own
authz, its own error mapping, and its own drift from the first; the invariant would then be a
promise about two code paths staying in step.

Three things fall out for free rather than being built:

- **Exactly-once (INV-E4).** `command_id` is a UUIDv7 and it *is* the `Idempotency-Key`. The
  gateway's reserve-first store (`kernel/api/idempotency.go`) already makes a replayed command
  return the original response with `Idempotent-Replayed: true`. A client that crashes between
  the server committing and the client recording the outcome re-sends and gets the same answer.
- **Accept-with-rebase (ADR-004 §Upstream 3).** Server-assigned values come back in the response
  body; applying that body to the replica is the patch. No separate rebase protocol.
- **Reject (INV-S4).** A non-2xx is already problem+json with a title, detail and status. The
  conflict tray renders what the server said instead of a translated approximation of it.

Zero new Go handlers. The only server change in this WP is §2.

## 2. The server accepts a client-supplied row id

`CRUD.Create` mints the id (`crud.go:115`), so an optimistically-applied offline create has no
final id until acceptance. That is the one place the "replay a stored HTTP request" design does
not close itself, because the row the user is looking at offline must become the row the server
has.

The alternative was a provisional local id rewritten on acceptance, with every pending command
that references it rewritten too. That is the causal-chain problem in full, it is the largest
single piece of new client logic the WP could contain, and it buys nothing: a row id is a
UUIDv7 surrogate key, not a sequence.

So `Create` uses `rec["id"]` when it is a valid UUIDv7 and generates one otherwise. Notes:

- **This is not INV-F6.** "Document number sequences are gapless-per-policy and assigned only at
  server acceptance" is about invoice numbers, and ADR-004 §Consequences' "offline documents
  carry a draft ID" is about the same human-visible identifier. Both are unchanged: document
  numbering still happens server-side at acceptance. This is the surrogate key underneath.
- **Today an `id` in the body is silently ignored** — `validated` iterates declared fields only,
  and `id` is not one. Silently discarding a caller's id is worse than either honouring it or
  refusing it, so this also closes a live wart.
- **A collision is a 409 with no detail.** The primary key is global per table, so a client that
  guessed another tenant's row id would otherwise learn it exists. `storage.IsUniqueViolation`
  already exists; the problem+json says the id is taken and nothing else.
- **A malformed id is a 422, not a silent regeneration.** A client that sends `"id": "banana"`
  has a bug, and inventing an id for it means the row it thinks it created is not the row that
  exists.

## 3. Optimistic rows are flagged in a sidecar table, not a column

docs/04 §Upstream 1 says rows are "flagged `pending`". The flag lives in `_pending
(object, row_id, command_id)` rather than as a column on every generated table.

The reason is the INV-S3 oracle. WP-2.2b-decisions §1 chose to mirror the server's physical
column shape precisely so the convergence comparison is a direct equality with no translation
layer inside it — "a bug in that layer is indistinguishable from convergence". A `_pending`
column on every replicated table would have to be excluded from that comparison, which puts
exactly the layer §1 rejected back into the oracle. A sidecar table is invisible to it.

It is also less code: no change to `createTableSQL`, `replicaFields` or `conform`, and the flag
survives its row being deleted, which a column cannot.

WP-2.2b-decisions §10 deferred per-field dirty tracking for the field-level master-data merge.
Still deferred, and still additive — it becomes columns on `_pending`, not on the replica.

## 4. Reconnect order: drain, then hydrate, then pull — and the purge runs last

The roadmap flags this as undesigned. The order is:

    open → drain → hydrate → pull        (and WP-2.4's revocation purge after pull)

- **Drain before pull.** Both orders are correct — the server revalidates against current state
  regardless of what the client knew (commandment 5) — so this is chosen on what the user sees.
  Pulling first overwrites the optimistic rows with server state that predates the user's own
  work, which makes their edits visibly disappear and then reappear a moment later. Draining
  first means the server sees the work, the feed carries it back, and the pull confirms it.
- **Purge after drain.** WP-2.4 queues a revocation purge; a purge that runs before a drain
  deletes rows that pending commands reference. This WP does not build the purge, it fixes the
  contract WP-2.4 must honour, and states it here so 2.4 does not rediscover it.
- **Drain before hydrate** matters only on a re-hydration after eviction, where there is nothing
  to drain anyway. It is in that position because the outbox is the one thing hydration cannot
  reconstruct, so it goes first on principle rather than on a case that arises.

## 5. Multi-tab needs no leader election in this WP

WP-2.2b-decisions §5 left coordination to 2.3, on the grounds that "an outbox with two writers
is a correctness problem and not merely a usability one". True — and the correctness half is
already enforced: the SAH-pool VFS holds an **exclusive** access handle, so a second tab cannot
open the replica at all, let alone write to it. 2.2b renders that as a distinct state.

There is therefore already exactly one writer. Web Locks leader election would let the *second*
tab work, which is a usability improvement, not a correctness one, and this WP does not need it.

What 2.3b owes is proof rather than machinery: a test that a second `Store` over the same OPFS
replica cannot open it, so the property the outbox rests on is asserted somewhere instead of
being a fact about a VFS we happen to have read the docs for.

<!-- ponytail: single-writer by exclusive VFS handle, not by election. Web Locks
     leader election lands when a second tab needs to work, not before. -->

## 6. Denied persistence: accept the write, cap the outbox, say so

WP-2.2b-decisions §7 deferred this: "WP-2.3 must decide what happens when persistence is
*denied* before it accepts a single offline write."

Refusing offline writes outright is the strictest reading of no-silent-loss and it is the wrong
trade. Chrome denies `navigator.storage.persist()` for sites without engagement, so the strict
rule would leave an ordinary tab with no offline capability at all — a certain loss of function
to avoid a probabilistic loss of data. Accepting silently is not an option either.

So, **when persistence is denied**:

1. A rendered, non-dismissable state says unsent work may be discarded by the browser. Not a
   toast — eviction clears the whole origin, so there is no afterwards in which to apologise.
   The warning has to be true before the first write, which is the only moment it can be.
2. The outbox is capped at `UNPERSISTED_OUTBOX_LIMIT` pending commands, and the UI shows
   `pending / limit` from the first write rather than at the limit. Reaching it blocks new
   offline writes with a distinct state — the same shape as §5's multi-tab state, and for the
   same reason: an opaque failure is the thing that is not acceptable.
3. The drain fires on `online` and on visibility-change, not only on the poll tick, so the
   window in which work is unsent is as short as the network allows.

**When persistence is granted there is no cap.** The limit exists to bound the blast radius of
an eviction, and a replica that cannot be evicted has no radius to bound.

The number: 50. It is a session's worth of offline edits — high enough that ordinary offline use
never meets it, low enough that meeting it is a signal rather than a surprise. It is a named
constant, asserted by a test, and shown to the user, which is what makes it honest; it is not
derived from anything, because nothing to derive it from exists yet.

<!-- ponytail: fixed 50-command cap when unpersisted. Derive from
     StorageManager.estimate() if real deployments start hitting it. -->

## 7. Outbox rows are parsed at the port boundary

`Store.query<T>` returns `T[]` by an unchecked cast, which is right for replicated rows — they
are overwritten from the server, so a malformed one repairs itself. It is wrong for the outbox,
which is the first thing in the client the server cannot reconstruct: a row that does not parse
is work that is gone, and casting means discovering that at the point of use.

So `parseOutboxRow(raw: unknown): OutboxCommand` validates shape and types and throws. This is
the same argument as `conform` (schema.ts) one table over, and the roadmap names it.

## 8. Invariants

- **INV-S1** *(no acknowledged write is ever lost, RPO 0)* — `Note` → `TestRequired`. Proven by
  the harness: commands are acknowledged only after the server's 2xx, and a client killed at
  every point in the drain either re-sends (and is deduped by INV-E4) or has already recorded
  the outcome. Never both, never neither.
- **INV-S2** *(offline commands pass the identical pipeline)* — `Note` → `TestRequired`. Held
  structurally by §1; the test asserts there is no route the drain can reach that the online UI
  cannot, i.e. that no sync-replay endpoint exists.
- **INV-S4** *(rejected commands surfaced, no silent drops)* — `Note` → `TestRequired`. Every
  outbox row ends in exactly one terminal state — accepted or filed as a conflict — and the
  property is that the count is conserved: commands in = accepted + conflicts, always.
- **INV-E4** — unchanged, and now load-bearing for the client: the drain's retry safety is the
  gateway's idempotency store.
- **INV-S3/S5** — unchanged, must not regress; 2.3a is the test that says so under faults.
- **INV-T2/T5** — unchanged. The drain carries the user's own credentials through the ordinary
  gateway, so an offline write is authorized as that user at the moment of acceptance, not at
  the moment it was queued. A permission revoked while offline rejects the command, which is the
  correct outcome and lands it in the tray.

## 9. Deferred, with reasons

- **Field-level master-data merge** (docs/04 §Conflict policy: "field-level merge when disjoint
  fields changed"). Needs per-field dirty tracking, which §3 keeps additive. Until then a
  same-row conflict goes to the tray whole. The tray is the documented fallback for this case
  anyway, so the gap is narrower than it sounds: it is "more trays than necessary", not "wrong
  merges".
- **Dependent-command chains** (ADR-004 §Upstream 4: a rejection cascades to dependents). The
  drain is strictly sequential and **stops at the first rejection**, leaving everything behind it
  pending. That is the conservative outcome — no command runs whose predecessor may have been
  its precondition — and it needs no dependency graph. The cost is that an unrelated command
  queued behind a rejected one waits for the user. <!-- ponytail: stop-on-reject, not a causal
  graph. Build the graph when real chains show it is costing users, not before. -->
- **Offline-capability matrix** (docs/04 §Offline capability matrix: posting to GL, payments and
  period close are online-required by default). Not enforced client-side in this WP; the server
  rejects them on drain and they land in the tray. Enforcing it locally is a better *experience*
  and belongs with the tenant-tunable knob it describes, which nothing yet reads.
- **Live push** — still WP-2.1 §7's standing deferral.
- **Replica encryption** — still WP-2.5, still unresolved as WP-2.2b-decisions §10 recorded it,
  and now holding user work the server has never seen, which raises its stakes without changing
  its answer.

---

# Addendum — decided while building WP-2.3b

§0–§9 were written in WP-2.3a, from the design. These four were not visible from there: each is
something the outbox turned out to need once it existed.

## 10. An optimistic row's `tenant_id` is learned from the replica, not from the session

Every generated table has `tenant_id TEXT NOT NULL` (schema.ts `KERNEL_COLUMNS`), so an
optimistically-applied create has to put *something* there, and the worker has no idea what. There
is no `GET /api/v1/sessions/current` — the tenant is returned once by `POST /api/v1/sessions` and
lives in the shell's memory, on the other side of the worker boundary.

Three options, and the cheap one is also the right one:

- **Pass it across the worker boundary at open.** Correct, and it makes every caller of
  `startReplica` responsible for knowing something none of them currently track.
- **Fetch it.** A new server route, on a WP whose §1 is "no new endpoints".
- **Learn it.** `applyRecord` already writes `tenant_id` on every row it lands. It records the
  value once, in `_sync_state.tenant`, and optimistic rows read it from there.

Learning it is chosen. The window where it is unknown is narrow to the point of theoretical: a
replica exists only after `/meta/objects` and a hydration, so a tenant would have to hold **zero
rows of every replicable object** and then write offline. In that window the optimistic row carries
`''`, and the server's own row overwrites it on acceptance (`ON CONFLICT DO UPDATE` covers
`tenant_id`). The INV-S3 oracle compares declared fields and `updated_at`, so it never sees the
difference either way — which is stated here rather than relied on silently.

## 11. A 409 from the idempotency store is a retry, not a rejection

§1 leans on the gateway's reserve-first store for exactly-once. What §1 did not account for is that
the same store answers **409** in two different situations (`kernel/api/gateway.go`):

- the key was reused with a *different* request fingerprint, and
- the key is **still in flight** — a reservation exists and no response is stored yet.

The second is the ordinary consequence of the crash §1 designed for. A client that dies after the
server began the write re-sends on the next drain, and if the original request is still executing it
gets a 409. Treating that as a rejection would file a conflict for a command that is at that moment
succeeding — inventing the user-visible failure INV-S4 exists to prevent, on the exact path INV-S1
is about. So the drain must retry it.

Telling the two 409s apart needs the server to say which it is, so the gateway now sets
`Type: "idempotency-conflict"` on that problem. This is the `capability-disabled` precedent
(`web/src/api/client.ts` already branches on a problem `type`), not a new mechanism.

Both 409s retry — a fingerprint mismatch cannot happen legitimately, since a `command_id` is minted
per command and never reused, so the honest reading of one is "something is wrong locally" rather
than "the server refused this work". What stops a retry loop is `_outbox.attempts`: past
`MAX_ATTEMPTS` the command is filed as a conflict like any other. An outbox entry that retries
forever is not a silent drop, but it is a silent *stall*, and the tray is where the user finds out.

<!-- ponytail: fixed attempt cap, no backoff schedule. The drain already only runs on
     reconnect/visibility, which is the backoff. -->

## 12. The drain orders by an integer `seq`, not by `command_id`

`command_id` is a UUIDv7 and UUIDv7s sort chronologically, so ordering by it is free and almost
right. Almost: two commands minted in the same millisecond order by their random bits. A PATCH
sorted ahead of the POST that creates its row is a 404, which the drain would file as a conflict —
a fabricated rejection for work that was perfectly valid, caused entirely by how the queue was
sorted.

`seq INTEGER PRIMARY KEY AUTOINCREMENT` is the same one column and orders by insertion. Laziness is
supposed to buy fewer moving parts, not a worse algorithm for the same price.

## 13. Screens still write online-first; the tray is what this WP wires in

This WP lands the outbox, the optimistic apply, the drain, the conflict tray and the pending /
unpersisted states. It does **not** route `ObjectForm`'s writes through the outbox.

The reason is the read path. Screens read through `web/src/api/client.ts`, not through the replica —
WP-2.2b left the replica read-only and WP-1.5-decisions §1 records the swap as a seam, not a
setting. A screen writing to the replica while reading from the server would show the user a form
that saves into a store it cannot see, which is worse than either end state. The swap is one change
to one file and it belongs with the WP that makes the replica the read path.

So the honest statement of what "offline is the default" means after this WP: the machinery is
there, proven end-to-end by the simulation harness against a real server, and one seam remains
before the shipped UI uses it.

Two of the tray's three affordances wait on that same seam. docs/04 §Upstream 3 describes "view
server state, edit & resubmit, or discard"; this WP ships **view and discard**.

- **Edit & resubmit** is `write` called with an edited body — the client side of it is one line.
  What it needs is a form to edit *in*, and the only form there is submits online. Wiring it to
  the tray before the screens write through the outbox would produce a button whose two paths
  disagree about where the change goes.
- **View server state** is deliberately narrower than it sounds here. The tray shows what the
  user tried and the server's own explanation of the refusal, which is the information the
  decision actually needs; fetching the current row adds a round trip, has nothing to show for a
  rejected create, and duplicates the object screen one click away.

Neither is a silent gap: INV-S4 is about a rejected command being *surfaced* and not dropped,
and both halves of that are here.

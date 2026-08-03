# WP-2.2 decisions — client replica

Roadmap: *"Client replica: SQLite-WASM/OPFS schema generation, hydration, incremental apply.
AC: replica-converges-to-projection property test."*
Design: [docs/04](../04-SYNC-ENGINE.md) §Downstream · [ADR-004](../adr/ADR-004-sync-model.md) ·
[ADR-017](../adr/ADR-017-sync-client-core.md) · [ADR-006](../adr/ADR-006-metadata-customization.md) ·
[docs/19](../19-DATA-INTEGRITY.md).

Unblocked: WP-2.1 (feed) and WP-1.11 (field validation + schema descriptors) are merged, and
ADR-017 chose the language. The prototype in `web/src/sync/` is this WP's seed, as that ADR
said it would be.

## 0. Split into two PRs

This WP is a new server read surface *and* an entire client replica. WP-1.6 hit the same
shape and split; so does this one.

- **WP-2.2a — server sync API.** §1 materialization, §3 per-object authorization, §4 paged
  snapshot, §5 archived-row conveyance. Entirely testable in Go against both dialects, with
  no client in the picture. Carries **INV-T1/INV-T2** on its new read paths.
- **WP-2.2b — client replica.** §6 schema generation, hydration, incremental apply on the
  WP-2.6 core, the ADR-017 worker boundary, and both convergence tests in §8. Flips
  **INV-S3** to `TestRequired`.

INV-S3 lands in B rather than A on purpose: convergence is a property of a replica, and A has
no replica to converge. Registering it against a server-only PR would mean writing a test that
asserts something about a component that does not exist yet — the kind of green tick that
makes a catalog less trustworthy, not more.

## 1. The gap WP-2.1 left, and how this WP closes it

**The change feed points; it does not carry.** WP-2.1 §3 made every entry a pointer
(`source` + `ref_id` into `events`/`audit_log`) so financial truth lives in one place. That
was the right call for storage and it leaves a client unable to apply anything: a replica
that learns "Invoice 019f… changed" still has no invoice.

Three ways to close it, and why the third wins:

- **Fetch each changed row by id.** One HTTP request per entry. A 500-change batch is 500
  round trips; on a phone that is the whole feature dead.
- **A batch row-fetch endpoint** the client calls after reading pointers. Two round trips per
  batch instead of one, and the client has to group ids by object and re-issue — work the
  server can do while it already holds the rows.
- **The server materializes on request** — `GET /api/v1/sync/changes?include=rows` resolves
  pointers into current row state, grouped by object, one query per object kind touched. One
  round trip, the storage design untouched. **Chosen.**

The feed on disk stays a pointer log. Materialization is a read-time join, not a second copy.

## 1a. Two WP-2.1 defects the first consumer exposed

Both found by building A, neither visible before there was anything reading the feed.

**The audit pointer pointed at the wrong row.** WP-2.1 §3 says "`source` + `ref_id` identify
the row in `events` or `audit_log`", and `recordAudit` implemented that literally: `ref_id`
was the *audit row's* id. For the event source that is right — the event is the thing. For
the audit source it is useless: the replica needs the **record that changed**, and an audit
id only reaches it by a second hop. Materialisation looked up contacts by audit id and found
nothing, which is how this surfaced.

`ref_id` for `SourceAudit` is now the record id. The audit row stays reachable by
`(tenant_id, object, record_id)` — the index `audit_log` already carries. This is a
pointer-target choice made in WP-2.1 with no consumer to check it against; A is that
consumer.

Existing feed rows written before this fix point at audit ids. Nothing consumes them: no
replica exists, and a client hydrates from a snapshot at the *current* cursor, so entries
older than its first snapshot are never materialised. No backfill.

**The feed serialised Go field names.** `Change`/`Entry` had no JSON tags, so the endpoint
WP-2.1 shipped returned `{"Cursor":1,"TenantID":…,"RefID":…}` — exported-field casing, unlike
every other payload in the API. Fixed to snake_case here rather than in B, because B is what
would have had to consume it.

## 2. Materialized rows are *current* state, not state-at-cursor — and that converges

A row that changed at cursors 5 and 9 materializes, at either cursor, to the state after 9.
The feed says *what* changed, not *what it was*. This is deliberate and it is worth being
explicit about why it is safe, because it looks wrong:

- Applying a row's state is **idempotent and last-writer-wins per row**. Applying the state
  from 9 while processing 5, then applying it again at 9, lands the same value.
- A client can therefore hold state *newer* than its own cursor claims. That is safe in one
  direction only, and this is the safe direction: it can be ahead, never behind. Any later
  change produces another entry, so it cannot get stuck ahead of a value that then moves.
- Convergence is to **current server state**, which is exactly what the AC asks for
  ("replica converges to projection"), not to a replay of history. History lives in the event
  log on the server, which is where ADR-004 says the referee is.

The consequence to accept honestly: **the replica is not a time machine.** It cannot
reconstruct what a row looked like at cursor 5, and nothing in Phase 2 asks it to. If a
client-side audit view ever needs that, it reads the event log over the API rather than
turning the replica into a second one.

## 3. Materialization must re-check authorization, per object

WP-2.1's threat notes leaned on the pointer design to bound blast radius: an over-broad
`sync:read` exposed "object names and row ids, not row contents". **This WP removes that
bound**, so the endpoint cannot inherit it.

Materialization authorizes **per object kind**, with the same `read` permission generic CRUD
enforces. A caller holding `sync:read` but not `Invoice:read` gets invoice *pointers* and no
invoice *rows*. That asymmetry is deliberate: the pointer is already the weaker disclosure
and suppressing it would make cursors non-contiguous for no gain, while the row is the thing
worth protecting.

Per-device scope narrowing (docs/04 §Concepts) is **WP-2.4**, unchanged. Object-level
permission is the whole filter until then, and the decisions file for 2.4 inherits the seam.

## 4. Hydration: snapshot at a consistent position

docs/04 §Downstream 5: "snapshot download (paged) at a consistent position, then stream from
there."

`GET /api/v1/sync/snapshot?object=X&after=<id>&limit=N` returns a page of rows ordered by
`id`, plus the feed's current high-water `cursor`. The client keeps the cursor from its
**first** page and resumes the feed there.

Paging a table that is being written to can show a mix of old and new rows, and that is fine
here rather than a bug to engineer around: every change after the recorded cursor is in the
feed, and applying it is idempotent per §2. A row that shifted mid-snapshot is repaired by
the first feed batch. The alternative — a repeatable-read snapshot held across every page —
means holding a transaction open for the length of a full hydration, which on a large tenant
is a long-lived reader in the write path.

`CRUD.List` cannot serve this: it returns every row for a tenant with no paging
(`kernel/metadata/crud.go:213`). Hydration of a real book would materialize the whole table
in memory on both ends.

## 5. Deletes must be conveyed, not omitted

CRUD soft-deletes (`archived_at`). `List` filters archived rows out, which is right for a UI
and wrong for a replica: a client that already holds a row and is told nothing keeps it
forever. **A divergence that no amount of syncing repairs is the one failure mode INV-S3
exists to catch**, so materialization returns archived rows too, flagged, and the apply loop
deletes them locally.

## 6. Schema generation from metadata, not from a hand-written mirror

The replica's tables are generated from `GET /api/v1/meta/objects` — the same effective
schema, per tenant, including overlays (ADR-006). CLAUDE.md forbids hand-written duplicates
of generated types, and ADR-017 rejected Rust largely on that ground; hand-writing the
replica's DDL here would break the same rule in the same component.

Field types map to SQLite affinities. Three are **excluded from the replica** in this WP,
each for a stated reason rather than by omission:

- `table` — a child-collection field; it is a second table with a parent link, and modelling
  child collections is its own design. Nothing in Phase 1 declares one.
- `computed` — derived on the server by definition; storing it client-side invents a second
  evaluator that can disagree.
- `file` — a pointer to blob storage; offline blobs are a Phase 4+ question (docs/04 says
  nothing about them, deliberately).

`money` stores minor units + currency as two columns, never a float — the same rule as the
server (commandment: integer minor units), and the reason the client already carries exact
strings through `web/src/meta/render.tsx`.

## 7. Invariants

- **INV-S3** ("client replica converges to server state; divergence is detected and repaired")
  flips from `Note: lands with WP-2.2` to `TestRequired: true`. This WP is what it was waiting
  for. Convergence is proven by property test (§8), not asserted.
- **INV-T1** — hydration and materialization are tenant-scoped read paths like any other; a
  replica must never receive another tenant's row.
- **INV-T5** — values arriving at the replica conform to the object's effective schema. The
  client does not re-validate business rules (the server is the referee) but it must not
  silently coerce: a value that does not fit its declared column is a bug to surface, not to
  round.
- **INV-S5** — unchanged from WP-2.1; the apply loop depends on it and the prototype's
  ordering tests come along.

Not claimed: **INV-S1/S2/S4**, which are upstream (outbox, command replay, conflict tray) and
land with **WP-2.3**. This WP is downstream-only.

## 8. The AC, and how it is actually tested

"Replica-converges-to-projection property test" gets two tests, because one of them alone
would be dishonest:

1. **Property, headless, many rounds.** Randomized operation sequences (create / update /
   soft-delete across objects, interleaved with sync at random points, including mid-batch
   interruption) against the real compiled `lasterp` binary, with the replica on
   `node:sqlite`. After each round: every replica table equals the server's current
   projection, row for row. This is the AC, and it is the shape WP-2.3's simulation harness
   extends.
2. **One pass in a real browser** over SQLite-WASM/OPFS in a worker, proving the same core
   converges on the shell that ships. ADR-017 §Consequences requires the worker; a
   Node-only proof would not exercise it.

## 9. Deferred, with reasons

- **Everything upstream** — outbox, optimistic apply, command replay, conflict tray: WP-2.3.
  This WP's replica is read-only, and says so in its API.
- **Scopes** — WP-2.4. Object-level permission is the filter until then (§3).
- **Event-sourced objects in the replica** (`JournalEntry`). The feed carries their pointers,
  and a replica of a stream is a different shape from a replica of a row. Ledger detail is a
  server read today (`modules/reporting`), and offline GL is not in M2's "work all day
  offline" story.
- **Live push.** The in-process notifier exists (WP-2.1); wiring it to a transport is the
  streaming-transport deferral WP-2.1 §7 already recorded. Polling converges; push only
  changes latency.
- **Replica encryption and remote wipe** — WP-2.5, explicitly.

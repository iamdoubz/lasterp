# WP-2.1 decisions — change feed service

Roadmap line: *"Change feed service: logical-replication tail → NATS, scope tagging,
cursored streams. AC: feed ordering + resume tests."*
Design: [docs/04-SYNC-ENGINE.md](../04-SYNC-ENGINE.md) · [ADR-004](../adr/ADR-004-sync-model.md) ·
[docs/19](../19-DATA-INTEGRITY.md).

Phase 2 is unblocked: WP-0.8 is merged, Phase 1 is complete (roadmap header, PRs #11–29).
This is the head of Phase 2 and depends on nothing outstanding. WP-2.6's ADR-017 decides the
*client* sync-core language and does not touch anything here.

---

## 1. The feed is a pointer log in the database, not a WAL tail

**The roadmap says "logical-replication tail". This WP does not build one.** Stating the
disagreement plainly, per CLAUDE.md, and then the reasoning:

- **It cannot satisfy the adapter rule.** Storage-touching code must pass the conformance
  suite on Postgres *and* SQLite. Logical decoding is a Postgres feature; SQLite has no
  equivalent. A WAL tail makes the entire downstream half of the sync engine
  Postgres-only, which contradicts ADR-005 (SQLite is a first-class solo-mode target) and
  would mean the offline product cannot be developed or tested in solo mode — the mode
  most contributors run.
- **It needs privilege the app deliberately gave up.** Logical decoding requires
  `wal_level=logical` and a role with `REPLICATION`. WP-1.10 just finished making the
  serving role *restricted* — it cannot even `INSERT INTO events` directly. Handing that
  role replication rights, or running a second privileged process, spends the guarantee
  `lasterp doctor` was built to assert.
- **The ordered log already exists.** docs/04 defines the change feed as "event store
  entries + CRUD audit entries + metadata changes ... Global position = `bigint` cursor".
  Those rows are already durable, already ordered, already tenant-scoped. Tailing the WAL
  to reconstruct a log the database is already storing in a table is a second copy of the
  truth, and two copies of financial truth can disagree.

What "logical-replication tail" was buying is **commit ordering**, which is real and is
handled in §2 rather than discarded. If a future WP needs sub-second latency at a scale
polling cannot serve, a WAL tail can be added *behind the same reader interface* as a
Postgres-only accelerator; nothing here forecloses it.

## 2. The hazard this WP exists to get right: commit order ≠ id order

`events.id` is `BIGSERIAL`. The migration comment calls it "the global, gapless, monotonic
cursor". **The gapless claim is false on Postgres**, and the failure it hides is the worst
one in the sync engine:

```
tx A: INSERT ... -> id 5      (still open)
tx B: INSERT ... -> id 6      COMMIT
reader: SELECT WHERE id > 4   -> sees 6, advances cursor to 6
tx A:                          COMMIT
reader: SELECT WHERE id > 6   -> never sees 5
```

Event 5 is committed, acknowledged to its writer, and **invisible to every replica
forever**. That is a silent loss of an acknowledged write — the exact thing INV-S1 forbids
— and it is not a rare race: it happens whenever two writes overlap, which under any
concurrency is constantly.

**Decision: make the interleaving unreachable, by taking a per-tenant ordering lock
immediately before allocating a cursor position.** `pg_advisory_xact_lock`, keyed by a hash
of the tenant id, released by commit. A writer therefore cannot take id 6 while another
holds id 5 uncommitted: it waits, and id order becomes commit order.

- **Postgres:** the advisory lock. Keyed *per tenant* because docs/04 specifies a totally
  ordered **per-tenant** log — two tenants writing at once have no ordering relationship to
  preserve and must not queue behind each other. A hash collision costs those two tenants
  throughput and never correctness, so no collision handling is needed.
- **SQLite:** nothing. It holds a write lock for the whole write transaction, so only one
  writer can hold an unassigned id at a time and id order is already commit order.

**Rejected: a snapshot horizon on `xmin`** (`xmin < pg_snapshot_xmin(pg_current_snapshot())`),
which was this file's first answer and is wrong. A row's `xmin` is its transaction's id,
assigned at that transaction's **first write**, while its feed id is assigned at the
**append** — and appends run *after* the write they describe, which is the whole point of an
outbox. The two orders therefore diverge in exactly our case: a still-running xid 105 can
hold feed id 5 while a committed xid 100 holds feed id 9, and the horizon would call 9
stable and strand 5. The ordering guarantee has to come from the sequence, not from xids.

Cost, stated plainly: **one feed append at a time per tenant.** The lock is taken
immediately before the INSERT and released by commit, so the window is the commit itself —
the transaction's real work has already happened by then — but it is a genuine ceiling on a
single tenant's write throughput. Carried as a `ponytail:` comment at the call site naming
the upgrade path: a per-scope lock (scope keys already exist; WP-2.4 fills them in), or a
Postgres-only WAL tail behind the same reader interface. Not a weaker ordering guarantee.

**This mints INV-S5** (§5). It is deliberately not folded into INV-S1: INV-S1 is the
end-to-end RPO-0 promise that only completes with upstream replay in WP-2.3, and claiming
it here would overstate what is proven. Same reasoning as WP-1.11 minting INV-T5 rather
than widening INV-T3.

## 3. One feed table, holding pointers rather than payloads

docs/04 specifies **one** cursor over three sources (events, CRUD audit entries, metadata
changes). `events.id` is `BIGSERIAL`; `audit_log.id` is `TEXT`. There is no shared order
today, and ordering a union by `recorded_at` is not stable — timestamps tie, and a resumed
reader would see a different order than the first pass, breaking the resume AC.

So: a new `change_feed` table, appended **in the same transaction** as the write it
describes (an outbox), carrying `(id BIGSERIAL, tenant_id, source, ref_id, object,
scope_key, recorded_at)`.

It stores a **pointer, not a copy**: `source` + `ref_id` identify the row in `events` or
`audit_log`, and readers hydrate from there in batches. Copying event payloads into the
feed would create a second copy of financial data that can drift from the first — a new
integrity surface, in the one part of the system where docs/19 says we get none. The cost
is a second query per batch, which is bounded and measurable.

Two append sites, both already choke points: `eventstore.Append` and the metadata CRUD
audit write. No module code changes.

## 4. Scope tagging is a seam here; WP-2.4 owns the engine

`scope_key` is written on every entry, but this WP computes it trivially — the object /
module name, tenant-wide. **Role-based scope computation, re-shaping and revocation purges
are WP-2.4's AC and are not built here**, because a scope engine with no client to shape
for is a speculative one, and WP-2.4 will know what it needs.

What this WP owes 2.4 is that the column, the index, and the tagging call site exist, so
2.4 replaces a function body rather than migrating a populated table.

## 5. INV-S5 — the new invariant

> **INV-S5** No committed change is skipped by the feed: for any cursor position, a reader
> observes every committed entry exactly once, in a stable total order that does not change
> on resume.

`LayerPipeline`, `TestRequired: true`. Registered in `kernel/integrity/catalog.go` and added
to docs/19 §1. INV-S1/S2/S3/S4 keep their existing "lands with WP-2.2/2.3" notes — this WP
does not flip them, since it builds no upstream path and no replica.

Also touched, not newly claimed: **INV-T1** (feed reads are tenant-scoped, and the new table
gets RLS + a policy like every other), **INV-T4** (entries inherit actor attribution from the
row they point at), **INV-E5** (feed determinism is the same purity property the event fold
relies on).

## 6. NATS carries a bell, not the data

The feed's transport of record is the database. NATS publishes only "tenant T has changes
past cursor N"; the subscriber then reads the durable feed exactly as a cold client would.

The alternative — publishing change payloads through JetStream — makes NATS a second
durable log requiring its own exactly-once and replay story, and puts financial data in a
second place it can be lost from or leaked from. As a bell, a dropped NATS message costs
*latency only*: the next poll finds the same rows, and INV-S5 stays a property of the
database rather than of a broker.

No ADR is needed for the dependency itself — docs/02 fixes NATS JetStream in the stack,
embedded in solo mode. Solo mode uses an in-process notifier and starts no broker; the NATS
publisher is for multi-node deployments, where there is genuinely a second implementation
rather than an interface invented for one.

## 7. Deferred, with reasons

- **The streaming transport** (WebSocket / gRPC-web, docs/04 downstream step 1). This WP
  exposes a cursored read over HTTP plus the notifier; that satisfies "every capability
  reachable via API/MCP" and the ordering/resume AC. A stream framing with no client to
  consume it would be designed twice — **WP-2.2** builds the replica that knows what it
  wants.
- **Backfill of pre-existing rows.** The feed starts empty and covers writes from its
  migration forward. Hydration of everything older is docs/04's "initial snapshot at a
  consistent position" — **WP-2.2**'s AC, and it reads the source tables, not the feed.
- **Feed retention/compaction.** The table grows monotonically. Same untracked-growth family
  as `idempotency_keys` (flagged in WP-1.12); both want one WP about retention, not a
  half-policy invented here.
- **Metadata-change entries.** `events` and CRUD audit are wired now. Schema/overlay changes
  (`object_schema_migrations`) are a third source docs/04 names; they land with WP-2.2, which
  is the thing that needs to re-shape a replica when a schema changes.

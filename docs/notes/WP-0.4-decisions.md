# WP-0.4 decisions & blockers

**Status: unblocked, in progress (2026-07-08).**

WP-0.4 (Event store) depends on WP-0.2 (storage adapters, merged PR #2) per
the roadmap's build-order diagram. It is a foundational Phase-0 WP (not a
module WP), so the WP-0.8 integrity-foundation gate ("never start a *module*
WP before WP-0.8") does not apply — same reasoning as WP-0.3.

## Invariants this WP touches (docs/19)

- **INV-E1** Streams are append-only; no UPDATE/DELETE on the events table.
- **INV-E2** Optimistic concurrency: version conflicts are rejected, never
  silently merged.
- **INV-E3** Events are immutable post-commit; schema evolution via
  upcasters only.
- **INV-E4** `command_id` is unique: replay/retry produces exactly-once
  effects.
- **INV-E5** Projections are pure functions of the log:
  `rebuild(events) ≡ current projection`, verified by checksum.

## Ambiguities resolved

**1. Scope of INV-E1 ("append-only... DB grants + triggers make it
impossible, not just forbidden").** docs/19's own build plan assigns "DB role
separation, append-only enforcement" to **WP-0.8** explicitly, as a
dedicated deliverable — not to whichever WP happens to create the `events`
table. Building full production role separation (a distinct low-privilege
app DB role with no direct UPDATE/DELETE grant on `events`) here would
duplicate WP-0.8's own AC and risks diverging from however WP-0.8
eventually formalizes it project-wide (it isn't the only table that will
need this treatment — WP-0.3's `sessions`/`users` etc. will too).
**Decision:** WP-0.4 adds a defensive Postgres trigger on `events` that
rejects any `UPDATE`/`DELETE` outright (belt, not the full suspenders) —
cheap, catches ordinary application bugs today — and registers this as a
partial INV-E1 enforcement. Full DB-grant-level role separation (closing
the "even a compromised/buggy privileged connection can't do it" gap) is
explicitly left to WP-0.8, called out in the PR so it isn't lost.

**2. Snapshots.** The WP line says "snapshots" with no shape specified.
Event-sourcing snapshot support here means: a `stream_snapshots` table
keyed by `(tenant_id, stream_id)` storing `(version, state jsonb,
recorded_at)`, and a `LoadStream` path that reads the latest snapshot (if
any) plus events after its version, rather than always folding from event
zero. **Decision:** the event store package provides the storage and
load-path; it does not itself decide *when* to snapshot (that's a
per-aggregate policy call for whichever module owns the stream, e.g. "every
100 events") — WP-0.4 exposes `SaveSnapshot`/`LoadSnapshot` as primitives,
with no automatic snapshot-taking policy baked in, since no aggregate
exists yet to make that call meaningfully.

**3. Upcasters.** "Upcasters migrate old shapes; events are never
rewritten" (ADR-003). Since no event schema has shipped yet (this WP
creates the very first one), there is nothing to upcast *from*. **Decision:**
ship the upcaster *mechanism* — a per-event-type registry of
`(fromVersion int) (upcastFunc func(json.RawMessage) (json.RawMessage, error))`
applied on read, keyed by `(type, schema_version)` stored on the event row —
proven by a test that registers a synthetic v1→v2 upcaster and confirms
`LoadStream` returns the upcasted shape. Real upcasters land with whichever
WP ships the first event schema that needs one (Phase 1 ledger work).

**4. Change feed cursor granularity.** docs/03's kernel table already
specifies `events(id bigserial, ...)` — a single global, gapless,
monotonic position across all tenants. This WP's "change feed with
cursors" AC is the read-side primitive over that column
(`ReadFeed(ctx, db, tenant, afterCursor, limit)`), not the NATS/WebSocket
streaming service — that's WP-2.1 (Phase 2), which docs/04 confirms
("Change feed: ... Global position = bigint cursor" is the shape; the
gRPC-web/WebSocket transport is explicitly Phase 2 scope). **Decision:**
WP-0.4 ships the cursor-based read query and the determinism test
(replaying from a cursor is a pure function of committed state); it does
not ship a subscription/push mechanism.

**5. `command_id` format.** docs/04: "client-generated `command_id`
UUIDv7" — sortable, time-ordered. **Decision:** use `uuid.NewV7()`
(already available in `github.com/google/uuid` v1.6.0, already a direct
dependency since WP-0.3) for `command_id` and any other UUID this WP mints,
rather than `uuid.NewString()` (v4, unordered) used in WP-0.3.

**6. WP-0.3's UUID versions.** Originally flagged here as an out-of-scope
known gap (WP-0.3's `tenants`/`users`/`sessions`/`roles` primary keys were
minted with `uuid.NewString()`, v4/unordered, not UUIDv7 as docs/03
specifies). Per explicit direction, fixed in this PR instead of carried
forward: added `kernel/idgen` (`idgen.New()` = `uuid.Must(uuid.NewV7())`) as
the one place that mints IDs, and switched every call site (`kernel/authz`,
`kernel/identity`, and their tests) to it.

**7. A more serious latent bug, found while building this WP's Postgres
tests.** None of WP-0.3's `kernel/identity`/`kernel/authz` repository
functions — nor this WP's first draft of `kernel/eventstore` — ever
actually set Postgres's `app.tenant_id` session variable before reading or
writing. They "worked" in WP-0.3's own test suite only because that suite
connected as the testcontainers superuser, which always bypasses RLS
regardless of `FORCE`. Under a real non-superuser role (what production
must eventually use), RLS's `USING` clause doubling as `WITH CHECK` means
`tenant_id = NULL` — every tenant-scoped read would silently return zero
rows and every write would be rejected. This surfaced immediately once
`kernel/eventstore`'s tests were switched to the non-superuser app-role
pattern (same pattern `kernel/tenancy`'s WP-0.3 RLS tests already used).
**Decision:** fixed at the root rather than patched per-call: added
`tenancy.WithTenant(ctx, db, tenant, fn)` — begins a transaction, calls
`SetContext`, runs `fn`, commits/rolls back — as the one correct way to run
any tenant-scoped query. Every exported function in `kernel/identity`,
`kernel/authz`, and `kernel/eventstore` now goes through it, and both
`kernel/identity` and `kernel/authz`'s Postgres tests were switched to the
non-superuser app-role pattern too, so this is actually verified rather
than assumed. `kernel/identity`'s `sessions` functions are the one
exception, correctly: `sessions` is deliberately RLS-exempt (WP-0.3
decision 7), so there's no RLS to satisfy there.

**8. SQLite `SQLITE_BUSY` under real concurrency.** The 1000-writer torture
test (INV-E2, this WP's own AC) hit `SQLITE_BUSY`/`SQLITE_BUSY_SNAPSHOT`
routinely even with a `busy_timeout` DSN pragma set and confirmed active —
a known limitation of `database/sql`-mediated concurrent access to a single
SQLite file, not something `busy_timeout` alone resolves. **Decision:**
`tenancy.WithTenant` retries the whole transaction (bounded, short backoff)
when `storage.IsBusy` — matching on the shared "database is locked" text,
since modernc.org/sqlite formats the two variants differently and only one
names `SQLITE_BUSY` — is true; any other error, including business errors
like `ErrVersionConflict`, still propagates immediately without retrying.
This is SQLite-only (`IsBusy` is always false on Postgres, which has no
equivalent transient-lock error for ordinary row writes) and benefits
every package that goes through `WithTenant`, not just `kernel/eventstore`.

## Storage layout

- `events(id BIGINT/BIGSERIAL PK, tenant_id, stream_id, version, type,
  schema_version, payload, actor_id, command_id UNIQUE, occurred_at,
  recorded_at)`, `UNIQUE(tenant_id, stream_id, version)` enforcing INV-E2 at
  the DB layer (a concurrent append with a stale version hits the unique
  constraint, not an app-level race).
- SQLite has no `BIGSERIAL`; per existing project convention (see
  `kernel/storage/migrate` dialect-tag mechanism from WP-0.3), the `events`
  table gets dialect-specific migration files: `INTEGER PRIMARY KEY
  AUTOINCREMENT` on SQLite (still a monotonic global cursor), `BIGSERIAL`
  on Postgres.
- RLS: `events` and `stream_snapshots` get the same tenant-isolation policy
  pattern as WP-0.3 (`ENABLE` + `FORCE ROW LEVEL SECURITY`), tested the same
  way (non-superuser Postgres role, per the WP-0.3 lesson already in
  memory).

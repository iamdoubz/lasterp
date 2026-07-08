# WP-0.5 decisions & blockers

**Status: unblocked, in progress (2026-07-08).**

## Dependency check

WP-0.5's textual dependency rule (docs/11-ROADMAP.md): "never start a
*module* WP before WP-0.8 is green." WP-0.5 (metadata engine) is listed in
**Phase 0 — Foundations (kernel skeleton)**, not Phase 1+ where the
roadmap's own vocabulary uses "module" (GL, AR/AP, CRM, payroll, tax —
docs/10-MODULES.md, Phase 1/4 WPs). WP-0.5 is kernel infrastructure, not a
module, so the WP-0.8 gate does not apply to it — same reasoning already
applied to WP-0.3 and WP-0.4.

**Ambiguity:** the roadmap's "Build order at a glance" ASCII diagram draws
a single merge bracket that visually includes the WP-0.8 and WP-0.7 lines
feeding into the same arrow as WP-0.3/WP-0.4 converging on WP-0.5, which
could be read as "WP-0.5 also depends on WP-0.8 and WP-0.7." **Decision:**
read the diagram as a rough layout of Phase 0 finishing before Phase 1
starts, not a literal per-WP dependency edge — the explicit textual rule
("module WP") is the authoritative signal per rule (4) ("the phase number
... wins over any per-doc build-plan note"), and WP-0.5's own AC has no
dependency on i18n (WP-0.7) or the integrity gauntlet CI stage (WP-0.8)
existing yet. WP-0.5's actual prerequisites — WP-0.2 (storage), WP-0.3
(identity/tenancy, for authz integration), WP-0.4 (event store, for
`persistence: event_sourced` awareness even though codegen targets CRUD
only this WP) — are all merged.

## Invariants this WP touches (docs/19)

- **INV-T1** No query path returns another tenant's rows — the generated
  CRUD engine and its tables follow the same `tenancy.WithTenant` + RLS
  pattern as every prior kernel package.
- **INV-T2** No write path executes without an authenticated principal
  and authorization decision — CRUD writes call `kernel/authz.Can` using
  the object's declared permission roles before mutating anything.
- **INV-T4** Every mutation is attributable — CRUD writes require an
  actor (via `kernel/authz.ActorFromContext`) and record it in `audit_log`.
- No existing INV-E/F/S/X entry covers "the audit trail for CRUD-domain
  writes is complete and append-only." See decision 6.

## Ambiguities resolved

**1. "CRUD codegen (Go handlers + validation)" — runtime engine, not
Go-source text generation.** A literal codegen pipeline (YAML → generated
`.go` files → compiled) is a much larger, more fragile undertaking (needs
`go/format`, template correctness across every field-type combination, a
build step wired into `make`) than the AC's actual proof point requires
("generated API passes generated conformance tests" is satisfiable either
way). **Decision:** implement a generic runtime CRUD engine
(`metadata.CRUD`, parameterized by an `EffectiveSchema`) operating on
`Record = map[string]any`, driven entirely by the schema's field
definitions — no `.go` files emitted. "Generated" describes behavior
produced from metadata at runtime, matching how ADR-006 itself describes
the effective schema feeding "storage DDL/migrations, validation, REST
endpoints, ... permission matrix" as one mechanism, not necessarily one
source-generation step per target.

**2. Scope: CRUD persistence only, not event-sourced.** The WP-0.5 AC line
explicitly says "CRUD codegen"; `docs/03`'s example schema shows a
`persistence: event_sourced | crud` field, but event-sourced object
handling (post-to-GL pipelines, workflow transitions, `kernel/eventstore`
integration) is real business logic that belongs with whichever module
first needs it (Phase 1 ledger). **Decision:** `metadata.Object` parses and
stores `persistence` for both values (so schema definitions are
forward-compatible), but `metadata.CRUD` (the generated engine) only
supports `persistence: crud` objects — attempting to build one for an
event-sourced object is a configuration error, not silently ignored.

**3. Scope: DDL generation is create-only, not full expand/backfill/contract
diffing.** docs/03's convention ("All schema evolution through metadata
migrations: expand → backfill → contract") implies diffing a new schema
version against the previous one to plan `ALTER TABLE` steps. The WP-0.5
AC's proof case is "define sample object in YAML" — a brand-new object,
version 1, with nothing to diff against. Full version-to-version diffing
needs a schema history store and a materially larger diffing algorithm.
**Decision:** WP-0.5 generates `CREATE TABLE` DDL (+ indexes + RLS policy)
for a schema's first version only. Diff-based `ALTER TABLE` planning for
evolving an already-deployed object's schema is a real, separate feature,
deferred to whichever WP first needs to add a field to a live tenant
object (flagged here rather than silently scoped out).

**4. Dynamic per-object DDL needs its own migration-tracking mechanism,
separate from `kernel/storage/migrate`.** That package's migration runner
is deliberately fixed at compile time (`//go:embed migrations/*.sql`) for
*kernel* tables (tenants, users, events, ...) — it cannot apply DDL
computed at runtime from a tenant's YAML-defined object, which is exactly
what the metadata engine needs to do when an admin defines a custom
object. **Decision:** the metadata engine owns a separate, small
migration-tracking table (`object_schema_migrations(tenant_id, object,
version, applied_at)`, itself a kernel table created via the normal
migration runner) and applies generated DDL directly via `db.ExecContext`
inside a `tenancy.WithTenant`-style transaction, recording the applied
version there. This keeps `kernel/storage/migrate` scoped to kernel schema
only, as it already is, and gives the metadata engine its own concern
without overloading the existing runner's compile-time-fixed design.

**5. Overlay scope: add-only for v1.** ADR-006 says overlays "may add
fields, add validations ..., adjust UI layouts, add states/transitions,
relabel, hide" but "may not: remove core fields, weaken core invariants
(double-entry, permission floors)." Implementing relabel/hide/UI-layout
adjustment has no bearing on WP-0.5's stated AC (DDL/CRUD/audit), which
only needs the two things that actually change *storage* and
*authorization* shape: adding fields, and permission-floor changes.
**Decision:** `metadata.Overlay` for WP-0.5 supports exactly two
operations — `AddFields` and `Permissions` (additive-only: an overlay may
grant a role an action it didn't have, never remove one the core
declared) — with explicit conflict detection: a field name colliding with
an existing field (core or an earlier-merged overlay) is
`ErrOverlayConflict`, and a permission map that omits a role the core
required is `ErrPermissionFloorLowered`. Relabel/hide/UI-layout/workflow
overlay operations are out of scope (no storage or authz consequence to
prove against yet) — flagged, not silently dropped.

**6. Audit trail as the concrete proof of INV-T4 for CRUD writes.** No
existing docs/19 invariant ID covers "the audit log is complete and
tamper-resistant" directly — 08-SECURITY-MULTITENANCY.md's "three
immutable trails" (event store, audit_log, agent_audit) is the source
requirement, not the numbered catalog. **Decision:** treat the audit
trail as the mechanism that discharges INV-T4 for CRUD-domain writes (as
`kernel/eventstore`'s append-only trigger already discharges the
equivalent concern for event-sourced writes), tag its tests INV-T4, and
give `audit_log` the same defensive append-only trigger pattern
`kernel/eventstore`'s `events` table already uses (WP-0.4) — full hash-chaining
for tamper evidence (08-SECURITY: "hash-chained per day") is a distinct
feature with no bearing on this WP's AC and is left to whichever WP
productizes the Integrity Gauntlet's runtime sentinels (WP-0.8/WP-6.7).

**7. YAML parsing.** `gopkg.in/yaml.v3` is already an *indirect* dependency
(transitively pulled in). Promoting it to direct for parsing Object schema
definitions — same reasoning as `google/uuid`/`golang.org/x/crypto` in
WP-0.3: no new module enters `go.sum`.

**8. Two bugs found and fixed mid-implementation (both caught by tests,
before merge, not shipped).**

- **Overlay fields cannot be physical columns on a shared table.** The
  first draft of `GenerateDDL` gave every field — core and overlay alike —
  its own column. That's wrong the moment two tenants overlay the same
  core object differently: one shared physical table (isolated by RLS +
  `tenant_id`, not by having its own copy per tenant) can't have a
  different column set depending on which tenant is asking. Caught
  immediately by `TestContactTenantIsolation` failing with "table already
  exists" the moment a second tenant tried to `ApplyDDL` the same object.
  ADR-006 already gives the correct answer, which the first draft missed:
  "Custom fields for core objects store in a JSONB column with generated
  typed accessors." Fixed: `Field.FromOverlay` (set by `Merge`, never by
  parsing core YAML) routes overlay fields into a fixed `custom_fields`
  TEXT column (a JSON blob) instead of a physical column; `metadata.CRUD`
  transparently reads/writes through it so callers don't need to know
  which fields are core vs. overlay.
- **`ApplyDDL`/`object_schema_migrations` were wrongly tenant-scoped.**
  Same root cause, deeper: applying an object's DDL is a one-time *global*
  operation (like `kernel/storage/migrate`'s own bookkeeping), not a
  per-tenant one, because the table it creates is shared. The first draft
  tracked "has this been applied" per `(tenant_id, object, version)`,
  so a second tenant's first `ApplyDDL` call still tried to `CREATE TABLE`
  a table the first tenant had already created — fixing the column issue
  alone didn't fix this. Fixed: `object_schema_migrations` dropped
  `tenant_id` entirely (`(object, version)` primary key, no RLS — it's
  engine bookkeeping, not tenant data, same category as
  `schema_migrations`), and `ApplyDDL`'s signature dropped its `tenant`
  parameter.

## Storage layout

- `object_schemas(tenant_id, name, layer, version, definition, checksum)` —
  kernel table (via `kernel/storage/migrate`), one row per layer per
  object per tenant (core-layer rows use a sentinel tenant marker since
  core schemas aren't tenant-specific — see implementation for the exact
  representation).
- `object_schema_migrations(object, version, applied_at)` — kernel table
  tracking which generated DDL has been applied; not tenant-scoped, no RLS
  (decision 4, corrected per decision 8).
- `audit_log(id, tenant_id, object, record_id, action, changes, actor_id,
  at)` per docs/03's kernel table list, RLS-isolated + `FORCE` + append-only
  trigger, same pattern as WP-0.3/WP-0.4.
- Generated per-object tables (e.g. the sample `Contact` object used to
  prove the AC) are shared across every tenant, isolated by `tenant_id` +
  RLS + `FORCE` — not one table per tenant. Core fields are physical
  columns; overlay fields live in a `custom_fields` JSON blob column
  (decision 8). `archived_at` for CRUD soft-delete (docs/03 convention).

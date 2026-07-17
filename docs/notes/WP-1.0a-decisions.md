# WP-1.0a decisions & scope — Metadata DDL evolution

**Status: unblocked, planned (2026-07-17).** Closes the create-only gap flagged
in WP-0.5 decision 3 and phase-0-review P1 #3.

## Dependency check
Phase 0 (WP-0.1–0.10, incl. the WP-0.8 integrity gate) is merged. WP-1.0a is the
first Phase-1 WP; it evolves the WP-0.5 metadata engine (`kernel/metadata`) and
touches no other module. Prerequisites — WP-0.2 storage, WP-0.5 metadata engine +
`object_schema_migrations`, WP-0.8 integrity gauntlet — are all in `main`.

## What the WP adds
`GenerateDDL`/`ApplyDDL` today are **create-only**: version 1 → `CREATE TABLE`, and
a second version of an already-applied object would try to `CREATE TABLE` again.
WP-1.0a adds **schema evolution**: diff the last-applied effective schema against
the new one, plan **additive** `ALTER` steps (expand-only within a version), apply
them, and **refuse destructive diffs** with a clear error.

## Invariants this WP touches (docs/19)
- **INV-T1** — the evolved table must keep its `tenant_id` + RLS + `FORCE`
  isolation; a migrated table returns zero cross-tenant rows exactly as the freshly
  created one did. The populated-round-trip test is tagged `//go:build integrity`
  and references INV-T1.
- **docs/03 convention** — "no destructive DDL within a major version;
  expand → backfill → contract." This WP enforces the *expand* half in code and
  refuses the *contract/destructive* half. There is no numbered INV for schema-
  migration integrity; this contributes to the gauntlet's **Migration integrity**
  suite (docs/19 §3). No new catalog entry is required (no numbered INV changes
  enforcement state).

## Ambiguities resolved

**1. "overlay" in the AC = evolving an existing object, not specifically a tenant
overlay layer.** Per ADR-006 + WP-0.5 decision 8, tenant/plugin **overlay** fields
live in the `custom_fields` JSON blob and produce **no** DDL — adding one is already
a data-safe no-op. The DDL path that actually risks data is a new **core/module**
schema version that adds or widens a *physical column*. WP-1.0a's evolution planner
targets that path; the AC's "add-field/widen-type … data intact" is proven against
core-column `ALTER`s (the risky path). Overlay-blob additions are covered as the
no-op-DDL case. Field identity is by name across versions.

**2. Diff at the column-type level, not the field-type level.** The field-type →
column-type mapping is many-to-one (money/decimal/percent/currency/text/email/… all
→ `TEXT`). A field-type change that maps to the same column type (e.g. `text` →
`money`) is a **no-op** DDL step. Steps are only emitted when the *SQL column type*
changes.

**3. Allowed (additive) evolutions, expand-only:**
- **Add a new *optional* core field** → `ALTER TABLE ADD COLUMN <c> <type>`
  (nullable). Pre-existing rows keep `NULL`; `Required=false` so no violation.
- **Widen a core field's column type** — allowed widenings (target losslessly holds
  the source): `BOOLEAN→INT`, `BOOLEAN→TEXT`, `INT→TEXT`, `TIMESTAMPTZ→TEXT`.
  - Postgres: `ALTER TABLE … ALTER COLUMN … TYPE <t> USING (<c>::<t>)`.
  - SQLite: **no-op** — SQLite uses type affinity and already stored the value
    flexibly, so the data round-trips without a physical alter (SQLite has no
    `ALTER COLUMN TYPE`). The planner validates the widening is legal on both
    dialects but emits SQL only for Postgres.
- **Add an index on a core field** → `CREATE INDEX` (additive).
- **Loosen `NOT NULL`** (`Required` true→false on an existing field) → drop-not-null
  (Postgres). Safe/additive. (SQLite: no-op; columns were created without a hard
  constraint beyond the initial `NOT NULL`, so a loosen is best-effort — see note.)
- **Add/remove an overlay field** → no DDL (custom_fields blob).

**4. Refused (destructive) evolutions → `ErrDestructiveDiff`, naming field + reason,
no partial apply:**
- Removing a core field (column drop).
- Changing a core field's column type to a non-widening (narrowing/incompatible),
  e.g. `TEXT→INT`, `INT→BOOLEAN`.
- Making an existing core field `Required` (`NOT NULL` on a populated table).
- Adding a **new `Required` core field** to a populated table (decision by Dan,
  2026-07-17): forces the author to add it nullable + backfill first, then enforce
  in a later contract step — no implicit nullable widening of intent.

**5. Schema history store = a snapshot column on `object_schema_migrations`.** To
diff vN+1 against the last-applied schema, `ApplyDDL` must know what shape it applied
before. Migration `0026` adds `applied_schema TEXT` (nullable) to
`object_schema_migrations`; `ApplyDDL` records the applied `EffectiveSchema` as JSON
at every version (create and evolve) and reads the highest applied version's snapshot
as the diff baseline. This keeps `ApplyDDL` self-contained (callers unchanged) and is
the "schema history store" WP-0.5 decision 3 said a real diff would need.

**6. Version ordering.** Versions apply in increasing order. `ApplyDDL(…, v)` where
`v` is already applied → no-op (idempotent, unchanged). Where the max applied version
> v (going backwards) → `ErrNonMonotonicVersion`. Where no prior version exists →
`CREATE TABLE` (unchanged create path).

**7. Still out of scope (flagged, not silently dropped):**
- Expression indexes on `custom_fields` overlay fields (ADR-006 "optional expression
  indexes") — still deferred; a performance feature with no bearing on the data-
  integrity AC. `Index:true` on an overlay field remains un-indexed.
- The **contract** step (enforcing `NOT NULL` after a backfill, dropping a column
  after deprecation) — needs a backfill mechanism and a major-version boundary;
  belongs with whichever WP first deprecates a shipped field.
- Renaming a field (would need an explicit rename map to distinguish from
  drop+add) — no AC need yet.

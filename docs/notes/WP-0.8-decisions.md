# WP-0.8 Integrity Foundation — Interpretation & Decisions

Scope per [docs/11-ROADMAP.md](../11-ROADMAP.md) and [docs/19-DATA-INTEGRITY.md](../19-DATA-INTEGRITY.md):
invariant registry, Integrity Gauntlet CI stage skeleton, adversarial writer
suite v1, DB role separation, append-only enforcement. **AC: every INV-E/INV-T
invariant has a tagged test that fails without enforcement.** This WP is the gate
blocking all Phase-1 module work.

## What already exists (WP-0.1 … WP-0.6, all on main)

- Append-only **triggers** on `events` (0012) and `audit_log` (0021) reject
  UPDATE/DELETE for *any* connection, including the owner — defensive, but not
  "impossible": the migration owner role is only stopped by the trigger, and the
  app role still holds UPDATE/DELETE grants. WP-0.4's own note flagged closing
  this "role has no grant at all" gap as WP-0.8's job.
- RLS (`ENABLE` + `FORCE`) + `tenancy.WithTenant` on every tenant-scoped table
  (INV-T1). `authz.Authorize` is the write choke point (INV-T2/T4);
  `ErrCorePermissionFloor` + overlay `ErrPermissionFloorLowered` guard INV-T3.
- **Every INV-E/INV-T already has ≥1 tagged test** (grep `INV-[ET]\d` over
  `*_test.go`): E1 append_only_test, E2 concurrency/eventstore_test, E3
  upcast_test, E4 eventstore_test, E5 feed/snapshot_test, T1 tenancy/metadata/
  eventstore/api/identity, T2 authz/metadata/api, T3 authz/overlay, T4
  authz/metadata. The AC is therefore largely met *by construction* — what's
  missing is the machinery that keeps it true.

## Decisions

1. **Registry = catalog as code, in `kernel/integrity`.** The full docs/19
   catalog (INV-F/E/T/S/X) is transcribed into a Go slice
   (`integrity.Catalog`). Each entry carries an ID, title, enforcement layer,
   and a `TestRequired` flag. Only invariants whose enforcing code exists *now*
   (all INV-E and INV-T) are `TestRequired: true`; INV-F/S/X are registered but
   `TestRequired: false` with a "lands with WP-x.y" note — their module WPs flip
   the flag when they add enforcement (docs/19: "each module WP adds its own").
   Registering them now makes the catalog the single source of truth without
   forcing tests for code that doesn't exist yet.

2. **The "CI fails if an invariant has no tagged test" gate is a repo grep.**
   `TestEveryRequiredInvariantHasATaggedTest` walks the repo's `*_test.go` files
   (excluding the checker itself) and asserts each `TestRequired` invariant's ID
   appears in at least one of them. Stdlib `filepath.WalkDir` + `regexp`; no new
   dependency, no per-package registration runtime (test binaries are separate
   processes, so a grep is both simpler and more robust than shared in-memory
   coverage state). Tagging convention: mention the `INV-xx` ID in a comment on
   the covering test. `ponytail:` — grep gate, upgrade to an AST/registration
   check only if a bare ID mention in an unrelated comment ever causes a false
   "covered".

3. **DB role separation = one owner-run helper, `EnforceAppendOnlyGrants`.**
   `REVOKE UPDATE, DELETE, TRUNCATE ON <protected tables> FROM <app role>`, where
   the protected-table list is derived from the catalog (invariants tagged
   append-only: `events`, `audit_log`). Postgres-only; **no-op on SQLite** (no
   role system — the trigger is the whole enforcement there, and solo mode is a
   single trusted process). This is the single source of truth a future
   deployment-provisioning WP (WP-10.x) calls; there is no production Postgres
   role provisioning in Phase 0 yet, so for now it is exercised and proven by the
   adversarial suite. This mirrors how WP-0.4 shipped the trigger as defensive
   and explicitly deferred role separation here.

4. **v1 role separation covers mutation of history (UPDATE/DELETE/TRUNCATE), not
   direct INSERT-bypass.** The app role keeps INSERT on `events`/`audit_log`
   because the pipeline (`eventstore.Append`, `recordAudit`) runs as that role.
   A raw INSERT that skips `Append` still cannot violate INV-E2 (the unique
   `(tenant_id, stream_id, version)` index rejects it) or INV-E1/E3 (it cannot
   alter existing rows). Making even INSERT reachable *only* through
   pipeline-owned `SECURITY DEFINER` functions (docs/19 layer 3, full form) needs
   the posting pipeline modelled as DB functions — deferred to Phase 1+/the
   ledger WP. Documented as a known boundary, not a gap in this WP's AC.

5. **Integrity Gauntlet = a dedicated CI job, skeleton form.** A new
   `integrity-gauntlet` job in `ci.yml` runs the `kernel/integrity` suite (the
   registry-coverage gate + the adversarial writer suite, both dialects via
   testcontainers). The invariant property/torture/golden suites named in docs/19
   §3 already run under the existing `test` job (`go test ./...`); the gauntlet
   job is the non-skippable integrity-specific gate and the seam the heavier
   nightly tiers (mutation, chaos, fuzz — explicitly WP-6.7) plug into later. Not
   re-running the whole matrix twice: `ponytail:` skeleton job, expand to a
   full parallel gauntlet matrix when the nightly tiers exist.

6. **Adversarial writer suite v1 targets the Phase-0 write surface only.** The
   docs/19 attack list includes unbalanced entries, closed-period posts, and
   float money — none of which have code to attack yet (ledger is WP-1.2). v1
   covers what exists: raw UPDATE/DELETE/TRUNCATE on `events` and `audit_log`
   (grant-denied on PG, trigger-rejected on both dialects), cross-tenant
   read/write (RLS, PG), duplicate `command_id` (exactly-once), and actor-less
   writes (INV-T2). Each module WP extends the suite with its own attacks (their
   ACs already say so).

## Invariants touched / registered

- **Enforced + tested now (`TestRequired`):** INV-E1, E2, E3, E4, E5, INV-T1,
  T2, T3, T4.
- **Registered, forthcoming (`TestRequired: false`):** INV-F1–F7 (Phase 1
  ledger/money/tax/inventory), INV-S1–S4 (Phase 2 sync), INV-X1–X5 (Phase 3
  plugins/AI, Phase 6 autonomy).

## ADR compliance

No new runtime dependency (stdlib + existing testcontainers/pgx/modernc).
Append-only + role separation + RLS align with ADR-003/005/007 and the docs/19
enforcement layers. `kernel/integrity` is invariant-enforcement code → belongs
under the CODEOWNERS + INV-X4 protection docs/19 §3 describes; wiring that
CODEOWNERS entry is noted for the repo-admin step (out of band of this code PR).

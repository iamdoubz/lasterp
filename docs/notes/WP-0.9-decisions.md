# WP-0.9 Capability Registry + Composability Solver — Interpretation & Decisions

Scope per [docs/11-ROADMAP.md](../11-ROADMAP.md) and [ADR-018](../adr/ADR-018-composability.md):
module manifests, dependency closure with user-visible preview, disable-without-
delete, profile presets skeleton. **AC: enable/disable closure tests;
disabled-module API returns `capability-disabled` problem+json; every shipped
profile boots in CI.** Phase 0; last WP before Phase-1 module work.

## What's already settled (so this WP is not speculative)

- [docs/10-MODULES.md](../10-MODULES.md) already carries the full provides/
  requires/enhances/reduced-mode table for M1–M9. Those capability contracts are
  architecture, not guesses — this WP transcribes them into machine-readable
  manifests. The modules' *code* lands in Phase 1+; their *manifests* land now,
  exactly as ADR-018 §consequences says ("docs/10 gains a provides/requires
  table per module; new modules must declare manifests or fail CI").
- Kernel services (identity/tenancy, metadata, command pipeline, event store,
  audit, sync, search, files, notifications, approval) are always-on and not
  disableable (ADR-018 §1).

## Decisions

1. **New package `kernel/capability`.** Manifest type + YAML parser (reuses the
   already-present `gopkg.in/yaml.v3`, same style as `metadata.ParseObject`),
   registry, solver, and the tenant enable-state store. No new dependency.

2. **Built-in manifests are embedded (`go:embed builtin/*.yaml`).** The M1–M9
   catalog from docs/10 ships as data inside the binary — the same "catalog as
   code/data" shape as WP-0.8's invariant catalog, and free of filesystem-path
   fragility across the three deploy shapes. When a real module is built it may
   migrate its manifest to `modules/<name>/manifest.yaml` and self-register;
   that move is a future refactor, not this WP. A test asserts every docs/10
   module has a well-formed manifest (the "declare a manifest or fail CI" gate,
   skeleton form).

3. **The solver is deterministic metadata evaluation over capabilities, not
   modules** (ADR-018 §3): `requires`/`enhances` are capability strings, finer
   than modules, so a light dep doesn't drag a whole module in. `EnableClosure`
   returns the transitive set of modules a request would additionally enable
   (for the preview string); `DisableImpact` returns the enabled modules that
   depend on the target (reverse deps, blocking disable with the reason shown).
   Enhance bridges auto-activate only when both sides are enabled and never add
   hard deps. Stable output ordering (sorted) so previews and tests are
   reproducible.

4. **Enable-state is per-tenant, DB-persisted, disable ≠ delete.** New table
   `module_state(tenant_id, module, enabled, mode)` — `tenant_id` first,
   RLS + FORCE, per the tenancy commandment. Enable/Disable toggle the `enabled`
   flag through `authz.Authorize` + `tenancy.WithTenant` and write an
   `audit_log` row (INV-T2/T4) — a capability change is an attributable
   mutation. "Disable" only flips the flag; no module data is touched (there is
   none yet), and re-enable restores visibility. Purge stays a separate explicit
   admin action (ADR-018 §5, out of scope here). A tenant with no rows has
   nothing enabled until a profile is applied.

5. **Gateway integration: a capability gate before CRUD dispatch.** The metadata
   REST gateway (`kernel/api`) gains an optional `CapabilityChecker`. For a
   registered object bound to a capability, the gate checks the tenant's
   enable-state; if the owning capability is disabled it returns RFC-7807
   `capability-disabled` (403, `type` slug `capability-disabled`) — explicit, not
   a confusing 403/404 (ADR-018 §5). Objects not bound to any module (kernel
   objects) are always allowed. The object→capability binding comes from an
   optional `objects:` list in the manifest; no change to the metadata `Object`
   schema (kept decoupled). Tested with a fixture manifest that owns an object,
   toggled off.

6. **Profiles = named module sets (skeleton).** The seven ADR-018 profiles
   (Personal, Accounting-only, CRM-only, Invoice-automation-only, Services SMB,
   Product SMB, Full suite) ship as embedded data: a name + the modules/modes to
   enable. **Seed data and role packs are explicitly WP-1.8** (ADR-018
   §consequences) — this WP ships the profile *definitions* + `ApplyProfile`
   (runs the solver closure, writes `module_state`). "Every shipped profile
   boots" = a CI test that, for each profile, the solver resolves a complete,
   satisfiable, kernel-floor-respecting closure with no missing `requires`, and
   `ApplyProfile` against a real DB (both dialects) leaves a consistent state.

7. **Reduced modes may reference forthcoming capabilities; base `requires` may
   not.** Base module `requires` must resolve to a provider in the catalog now
   (the closed graph profiles boot on). A reduced mode's extra `requires` may
   name a capability no shipped module provides yet — e.g. payables'
   `capture_export` mode needs `documents.ocr`, which lands with M3/OCR. Such a
   mode is reported *unavailable* by the solver until its provider exists, and a
   test asserts exactly that. Consequently **6 of the 7 ADR-018 profiles ship
   bootable now** (Personal, Accounting-only, CRM-only, Services SMB, Product
   SMB, Full suite); **Invoice-automation-only is defined but flagged
   `Pending: documents.ocr` and excluded from the bootable set** — its whole
   point is the OCR capture→export mode, so shipping it before OCR would be a
   lie. It flips to bootable when M3 provides `documents.ocr`. "Every shipped
   profile boots" holds over the bootable set. Country-pack gating for payroll
   (docs/10 M7) is an enablement gate (a pack/plugin), not a capability edge, so
   payroll's graph `requires` is `[hr.core, ledger.core]`.

8. **Composability gauntlet hook.** WP-0.8 gave us the registry + gauntlet job.
   The profile-boot test + closure tests are the "composability suite" ADR-018
   §7 asks for, run under the existing `test`/`integrity-gauntlet` CI. No new
   invariant IDs (INV-F5 already makes money-without-ledger impossible); the
   solver's integrity floor (can't disable kernel; money modules require
   `ledger.core`) is asserted by tests, and registered as forthcoming coverage
   only if a new INV is warranted (it is not — this is policy over existing
   invariants).

## Deliberately deferred (not gaps in this WP's AC)

- Profile **seed data + role packs** → WP-1.8.
- The setup interview / size-tier recommendation (docs/14, docs/22) → product WP.
- `modules/<name>/manifest.yaml` self-registration → when the first real module
  is built (Phase 1).
- Disabled-module effects beyond the API gate (nav, MCP catalog, search, sync
  scope filtering) → those subsystems don't exist yet; each wires the same
  `IsEnabled` check when it lands (MCP WP-3.4, sync Phase 2, search WP-3.5).
- Purge (hard delete of a disabled module's data) → separate audited admin WP.

## ADR / commandment compliance

No new runtime dependency. `module_state` gets `tenant_id`-first indexing + RLS.
Enable/disable is attributable + audited. Solver cannot disable kernel services
or violate the integrity floor. Module boundaries (no lateral imports) are what
make the manifest cheap — the manifest just formalizes them.

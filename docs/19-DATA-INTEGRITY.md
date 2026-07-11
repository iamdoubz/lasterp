# 19 — Data Integrity: The Paramount Requirement

**Founding requirement (Dan, 2026-07-07):** data integrity is paramount. No new feature, AI enhancement, or plugin may be able to ruin the integrity of our data — and this must be proven by exhaustive tests, not promised by review.

This doc consolidates integrity guarantees scattered across ADR-003/005/007/014 into one enforceable system: a numbered **invariant catalog**, four **enforcement layers**, and the **Integrity Gauntlet** — a CI stage that every change, human- or AI-authored, must pass.

## 1. The invariant catalog (numbered, versioned, machine-checked)

Every invariant below has: an ID, an enforcement layer, a test suite tagged with its ID, and a runtime sentinel where applicable. New modules MUST register their invariants here; CI fails if an invariant has no tagged tests.

**Financial (INV-F):**
- INV-F1 Every journal entry balances: Σdebits = Σcredits, per currency, to the minor unit.
- INV-F2 Posted financial documents are immutable; corrections are reversing/compensating events only.
- INV-F3 No posting into a closed period; period close is monotonic (reopen = privileged, audited, reversible event).
- INV-F4 Money is integer minor units + ISO-4217; no floats anywhere in a money path; allocation never creates or destroys a cent (Σparts = whole).
- INV-F5 Every financially-relevant document posts to GL through its declared template; no direct ledger writes outside the posting pipeline.
- INV-F6 Document number sequences are gapless-per-policy and assigned only at server acceptance.
- INV-F7 Stock quantity × valuation reconciles with GL inventory accounts at all times (projection lag bounded).

**Event store (INV-E):**
- INV-E1 Streams are append-only; no UPDATE/DELETE on the events table (DB grants + triggers make it impossible, not just forbidden).
- INV-E2 Optimistic concurrency: version conflicts are rejected, never silently merged.
- INV-E3 Events are immutable post-commit; schema evolution via upcasters only.
- INV-E4 command_id is unique: replay/retry produces exactly-once effects.
- INV-E5 Projections are pure functions of the log: rebuild(events) ≡ current projection, verified continuously by checksum.

**Tenancy & access (INV-T):**
- INV-T1 No query path returns another tenant's rows (RLS as backstop; zero rows without tenant context).
- INV-T2 No write path executes without an authenticated principal and authorization decision.
- INV-T3 Permission floors and approval gates cannot be lowered by overlays, plugins, or agents (ADR-014 constitution).
- INV-T4 Every mutation is attributable: actor, command, timestamp — no anonymous writes, including system/agent/plugin writes.

**Sync (INV-S):**
- INV-S1 No acknowledged write is ever lost (RPO 0).
- INV-S2 Offline commands pass the identical validation pipeline as online writes — no privileged sync side door.
- INV-S3 Client replica converges to server state; divergence is detected and repaired, never ignored.
- INV-S4 Rejected commands are surfaced to the user; no silent drops.

**Extension & autonomy (INV-X):**
- INV-X1 Plugins touch data only via capability-checked host functions — no ambient authority; a plugin cannot violate INV-F/E/T/S even if maliciously constructed.
- INV-X2 Plugin/hook failure never corrupts or partially commits a transaction.
- INV-X3 Agent/AI writes go through the same command pipeline, permissions, and approval gates as human writes — always (no "AI mode" bypass).
- INV-X4 No autonomous process (L0–L4) can modify invariant-enforcement code, this catalog, or its test suites.
- INV-X5 Migration/import writes obey every invariant above; bulk paths get no shortcuts (they get batching, not bypasses).

## 2. Enforcement layers (defense in depth — a violation must beat all four)

1. **Type system & codegen:** money types, tenant-scoped repositories, generated parameterized queries — entire bug classes unrepresentable.
2. **Storage layer:** DB constraints, RLS, append-only grants/triggers on event tables, CHECK constraints for balance where expressible. The database refuses what the application forgot.
3. **Command pipeline:** single choke point for all writes (UI, API, sync replay, plugins, agents, migrations). If a write didn't come through it, it didn't happen — enforced by DB role separation (app role has no direct DML on protected tables outside pipeline-owned functions).
4. **Runtime sentinels:** continuous verification (docs/13) — projection checksums vs event-fold, trial-balance audits, orphan scans, cross-tenant canary probes. Detection is a feature: a violation that slips through layers 1–3 is caught in minutes, quarantined, and auto-filed as a P0.

## 3. The Integrity Gauntlet (CI — exhaustive by policy)

A dedicated, non-skippable CI stage. **Every PR runs it. Every plugin submission runs it. Every L2/L3/L4 autonomous change runs it. No green gauntlet, no merge/install/apply — no exceptions, including hotfixes.**

| Suite | What it does |
|---|---|
| Invariant property tests | Randomized operation sequences (property-based, high iteration count nightly) asserting every INV-* holds; each invariant ID must have ≥1 tagged property |
| Adversarial writer suite | Attempts every known bypass: raw SQL to protected tables, unbalanced entries, closed-period posts, cross-tenant reads/writes, UPDATE on events, float money, pipeline-skipping writes — all must fail |
| Hostile plugin suite | Malicious WASM corpus (exfiltration, capability escalation, resource bombs, partial-commit attempts) against INV-X1/X2 |
| Hostile agent suite | Red-team agent scenarios (prompt-injected goals: "bypass approval", "post directly", "widen own permissions") against INV-X3/X4 |
| Sync simulation harness | N clients × partitions/crashes/reorderings × INV-S properties (docs/04) |
| Concurrency torture | Parallel writers on hot streams/documents; INV-E2/E4 under contention |
| Fuzzing | All parsers (bank files, imports, API payloads, plugin manifests): malformed input may be rejected, never corrupt state |
| Golden files | Tax, payroll, report, posting-template calculations vs certified expected outputs |
| Adapter conformance | Full suite on Postgres AND SQLite — identical semantics or the build fails |
| Migration integrity | Every schema migration round-trips on seeded data with pre/post invariant checks + checksum manifests |
| Mutation testing (nightly) | Mutate invariant-enforcement code; if the test suite doesn't catch the mutant, the suite is inadequate — build fails |
| Chaos (nightly) | Kill nodes/DB failover mid-commit; INV-S1/E1 must hold after recovery |

**Coverage policy:** invariant-enforcement code requires 100% branch coverage + mutation score threshold (≥85%); "never weaken/delete a failing invariant test" (CLAUDE.md) is enforced by CODEOWNERS on `kernel/integrity/` + the gauntlet definitions — changes there need two human maintainer approvals and are outside every autonomous path (INV-X4).

## 4. Runtime posture
Sentinels run in production on schedule + sampling; any INV violation → automatic quarantine of the offending path, P0 task with forensic bundle (docs/12), tenant-visible integrity status page. The event log + hash-chained audit trails make every incident reconstructable. Integrity incidents are the only class where the system is biased to *stop* rather than degrade gracefully.

## Build plan
- **WP-0.8 Integrity foundation (Phase 0, blocks all module work):** invariant registry (catalog as code), gauntlet CI stage skeleton, adversarial writer suite v1, DB role separation, append-only enforcement. AC: all INV-E/T invariants have tagged failing-by-default tests that pass only with enforcement in place. *Shipped in [`kernel/integrity`](../kernel/integrity/): the catalog (`Catalog`, this doc §1 as code), `EnforceAppendOnlyGrants` (the §2 role-separation layer for `events`/`audit_log`), the `TestEveryRequiredInvariantHasATaggedTest` registry gate, and the adversarial writer suite — run as the non-skippable `integrity-gauntlet` CI job. Scope decisions in [docs/notes/WP-0.8-decisions.md](notes/WP-0.8-decisions.md).*
- Each module WP adds its INV-F/S/X properties to the gauntlet as part of its own AC (already reflected in module ACs; this doc is the umbrella).
- **WP-3.9 Hostile agent suite** (with MCP server WP-3.4) · **WP-6.7 Mutation testing + sentinel productization** (with Phase 6).

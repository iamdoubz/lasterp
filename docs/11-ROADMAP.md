# 11 — Roadmap & Agent Work Packages

Phases are sequential; work packages (WP) within a phase parallelize across agents. Every WP lists acceptance criteria (AC) — a WP is done when ACs pass in CI, not before. Conventions in [CLAUDE.md](../CLAUDE.md).

## Build order at a glance (start here)

**Nothing is built yet. The very first task is WP-0.1.** The critical path is:

```
WP-0.1 repo bootstrap
  → WP-0.2 storage adapters ──→ WP-0.4 event store ──┐
  → WP-0.3 identity/tenancy ──────────────────────────┤
  → WP-0.8 INTEGRITY FOUNDATION (gate: blocks all     ├→ WP-0.5 metadata engine
     module work; docs/19)                             │   → WP-0.6 API gateway
  → WP-0.7 i18n kernel (parallel, low-dependency)     ─┘
→ Phase 1 (ledger → tax → invoicing → web client → reports/metrics → role dashboards)
→ Phase 2 (sync engine; WP-9.1 adapter contract extracted here)
→ Phase 3 (plugins → automations → MCP/AI → dev platform)
→ Phase 4 (AP, banking, CRM, inventory, work mgmt, connectors, migration wave 1,
           privacy engine, dashboards GA, topology bundles)
→ Phase 5 (scale hardening, payroll, migration wave 2, compliance packs,
           DB adapters MSSQL/MySQL, Valkey, HA references)
→ Phase 6 (self-evolution L0–L4, shadow tenants*, region DR)
```
\*Exception: WP-6.3 (shadow tenants) may be pulled forward to Phase 4 if migration validation (WP-7.3) needs it before Phase 6 — it has no Phase-5/6 dependencies.

**Dependency rules for agents:** (1) never start a module WP before WP-0.8 is green; (2) numbered tracks 7–10 (migration, compliance, portability, topology) are *cross-phase tracks* — their WPs state which phase they land with; (3) within a phase, WPs are parallel unless one's AC references another's output; (4) when in doubt, the phase number in this section wins over any per-doc build-plan note.
**Superseded WPs (do not build):** WP-4.6 → WP-7.4 · WP-5.5 → WP-7.7 · WP-5.7 → WP-8.2.

## Phase 0 — Foundations (kernel skeleton)
- **WP-0.1 Repo bootstrap:** monorepo layout per 02-TECH-STACK.md; Go workspace; pnpm workspace; CI (lint, test, build all three deploy shapes); devcontainer; `lasterp dev` command; licensing per ADR-012 (AGPLv3 root LICENSE, Apache-2.0 in sdk/ + proto/ + plugin ABI dirs, SPDX header lint, DCO check). AC: fresh clone → `make dev` serves hello-world API + web shell in <5 min; license lint green.
- **WP-0.2 Storage adapters:** Postgres + SQLite adapters behind one interface; migration runner (expand/contract discipline); testcontainers harness. AC: identical adapter conformance suite passes on both.
- **WP-0.3 Identity & tenancy:** tenants, users, sessions, OIDC + password/TOTP, RBAC core, RLS policies + middleware tenant context. AC: no-context-zero-rows suite; authz property tests.
- **WP-0.4 Event store:** append with optimistic concurrency, snapshots, upcasters, change feed with cursors; SQLite + Postgres. AC: concurrency torture test (1000 writers, one stream, zero lost/dup events); feed replay determinism test.
- **WP-0.5 Metadata engine v1:** object schema parser/validator, overlay merge algorithm, DDL generation + migration planning, CRUD codegen (Go handlers + validation), audit logging. AC: define sample object in YAML → generated API passes generated conformance tests; overlay conflict detection cases.
- **WP-0.6 API gateway:** REST routing from metadata, OpenAPI generation, idempotency keys, problem+json errors, rate limiting. AC: OpenAPI spec validates; idempotent replay returns identical result.
- **WP-0.7 i18n kernel** (string layer, ICU, locale formatting, RTL-safe UI kit foundations — docs/17). AC: pseudo-locale + RTL build renders; hardcoded-string lint gate.
- **WP-0.8 Integrity foundation** (invariant registry, Integrity Gauntlet CI stage, adversarial writer suite, DB role separation, append-only enforcement — [19-DATA-INTEGRITY.md](19-DATA-INTEGRITY.md)). Blocks all module work. AC: all INV-E/T invariants have tagged tests that fail without enforcement.
- **WP-0.9 Capability registry + composability solver** ([ADR-018](adr/ADR-018-composability.md)): module manifests, dependency closure with user-visible preview, disable-without-delete, profile presets skeleton. AC: enable/disable closure tests; disabled-module API returns capability-disabled problem+json; every shipped profile boots in CI.

## Phase 1 — Ledger + first vertical slice (usable accounting MVP)
- **WP-1.1 Money & currency core:** money type (minor units), decimal policy, currency registry, FX rate store + ECB provider. AC: property tests on rounding/allocation (no lost cents in splits).
- **WP-1.2 Ledger module:** accounts, journal entries, periods, posting pipeline with storage-enforced invariants. AC: unbalanced/closed-period/mutation attempts all rejected at storage layer; trial balance projection matches event fold under fuzzing.
- **WP-1.3 Tax engine v1:** jurisdictions, rules, effective-dated rates as data; document tax calculation; US state + EU VAT seed packs. AC: golden-file test suite of tax scenarios.
- **WP-1.4 Invoicing/AR module** (per 10-MODULES.md M2, minus payments/dunning). AC: invoice lifecycle e2e — draft → post → GL entries correct → PDF renders.
- **WP-1.5 Web client v1:** metadata-rendered list/form/detail, auth, navigation, LastERP UI kit foundations. AC: Playwright e2e on invoice lifecycle; p95 budget smoke.
- **WP-1.6 Reports v1 + metrics layer** (amended per [21-REPORTING-DASHBOARDS.md](21-REPORTING-DASHBOARDS.md)): trial balance, P&L, balance sheet, AR aging from projections; CSV/XLSX export; metrics layer v1 + drill-down. AC: report/metric totals reconcile with event-fold oracle; permission-leak suite green.
- **WP-1.8 Role dashboard packs v1** (CEO/CFO/AR/AP, KPI cards with mandatory comparisons — docs/21). AC: fresh tenant shows live role dashboard from seed data.
- **WP-1.7 Translation packs + localized documents** (docs/17). AC: invoice e2e fully localized incl. PDF in first non-English locale.
- **Milestone M1: "a small firm can invoice and keep books"** — dogfood tenant live.

## Phase 2 — Sync & offline
- **WP-2.1 Change feed service:** logical-replication tail → NATS, scope tagging, cursored streams. AC: feed ordering + resume tests.
- **WP-2.2 Client replica:** SQLite-WASM/OPFS schema generation, hydration, incremental apply. AC: replica-converges-to-projection property test.
- **WP-2.3 Outbox & command replay:** optimistic apply, pending flags, replay pipeline, accept/reject/rebase, conflict tray UI. AC: simulation harness (04-SYNC-ENGINE.md test plan) green; no-silent-loss property.
- **WP-2.4 Scope management:** role-based scope computation, re-shape on change, revocation purge. AC: entitlement-change scenarios.
- **WP-2.5 Device security:** device registration, replica encryption, remote wipe. AC: wipe honored on reconnect; replica unreadable without keystore.
- **WP-2.6 Spike: shared sync-client core** (TS lib vs Rust/WASM lib for web+Tauri+mobile). Output: ADR-017.
- **Milestone M2: "work all day offline, sync perfectly"** — the Lotus Notes demo.

## Phase 3 — Extensibility & AI
- **WP-3.1 Plugin host:** wazero + Extism integration, manifest/capabilities, hook points, resource limits, circuit breakers. AC: hostile-plugin test suite (infinite loop, memory bomb, exfiltration attempt) all contained.
- **WP-3.2 PDKs + registry v1:** Rust/Go/TS/Python scaffolds, typed bindings codegen, signed bundles, install flow. AC: afternoon-plugin tutorial completes; example plugins (commission calc, Slack notifier) pass.
- **WP-3.3 Automations:** trigger/condition/action workflows as metadata; CEL expressions. AC: automation e2e suite.
- **WP-3.4 MCP server:** tool generation from metadata, module task tools, agent principals, budgets, approval gates, agent audit. AC: scripted agent closes a demo month-end with approval gates exercised; access-control red-team suite.
- **WP-3.5 Semantic layer:** pgvector embeddings pipeline, semantic search API, duplicate detection. AC: recall benchmarks on seeded data; degrades cleanly with no model configured.
- **WP-3.6 UI extension slots + sandboxed iframe bridge.** AC: untrusted widget cannot escape bridge (security tests).
- **WP-3.9 Hostile agent suite** (red-team scenarios vs INV-X3/X4, docs/19; runs with WP-3.4).
- **WP-3.7 Developer platform v1** (portal, OAuth 2.1 + PAT + scopes, live tenant OpenAPI spec, Spectral CI gate) · **WP-3.8 Postman & SDK pipeline** — see [15-API-DEVELOPER-PLATFORM.md](15-API-DEVELOPER-PLATFORM.md). WP-3.4 AC amended: Streamable HTTP + OAuth 2.1 resource-server conformance.
- **Milestone M3: "extend it in an afternoon; an agent can run it; a third-party dev integrates in a morning"**

## Phase 4 — Breadth
- WP-4.1 Payables/AP · WP-4.2a–d Banking & reconciliation (statement ingestion, match engine + workbench, payment initiation, aggregator/PSP connectors — [18-BANKING-FINANCIAL-INTEGRATION.md](18-BANKING-FINANCIAL-INTEGRATION.md)) · WP-4.3 CRM lean · WP-4.4 Inventory · WP-4.5 Connector framework + first connectors (Outlook/Graph, Salesforce, Stripe, bank aggregator) · WP-4.7 Tauri desktop. (WP-4.6 importers superseded by Migration Factory WP-7.4.)
- **WP-4.8 Work management core** (WorkItem/Project/views/linkage/inbox) · **WP-4.9 Docs & embeds** · **WP-4.10 Forms + live-measure Goals** — see [12-WORK-MANAGEMENT.md](12-WORK-MANAGEMENT.md) · **WP-4.11 Sandbox self-service** (docs/15) · **WP-4.12 E-invoicing adapter framework** (PEPPOL + two national mandates, docs/17).
- **Migration Factory wave 1 (docs/16):** WP-7.1 staging lake + profiler · WP-7.2 mapping engine + review UI · WP-7.3 reconciliation suite · WP-7.4 QuickBooks/ERPNext/Odoo packs.
- **WP-8.1 Privacy engine** (PII enforcement, purpose registry, DSAR workflows, crypto-shredding, retention, consent — [20-COMPLIANCE-PRIVACY.md](20-COMPLIANCE-PRIVACY.md)).
- **WP-4.13 Dashboard builder GA** (no-code grid, chart intelligence, live feed subscription, sharing/TV mode — docs/21).
- **WP-4.14 HR / Employee Directory (M10)**: employee records + number series, org tree/chart, effective-dated position history, certifications with expiry→work-item automation, optional employee↔user link + self-service, PII field masks. AC: standalone profile (HR only, no ledger) boots and passes e2e; user-link/unlink audited; expiring cert files a task; payroll (WP-5.3) consumes hr.core with zero employee-object duplication.
- **Milestone M4: "replaces QuickBooks — and ClickUp — outright; migrates you off them; talks to your stack"**

## Phase 5 — Scale & payroll
- WP-5.1 50k-load hardening (nightly full-scale test, budget enforcement) · WP-5.2 Citus/partitioning path validated · WP-5.3 Payroll kernel + US country pack (gated) · WP-5.4 SAP CPQ + ServiceNow connectors · WP-5.6 prose-CRDT spike (Yjs, also serves M9 docs) · WP-5.8 Dashboards v2 (NL assembly, annotations, cross-filtering, embeds, remaining role packs — docs/21) + time→billing bridge · WP-5.9 instant payment rails + EBICS + screening hook (docs/18). (WP-5.7 superseded by WP-8.2.)
- **Compliance track (docs/20):** WP-8.2 controls matrix + evidence automation + `lasterp compliance report` (ISO 27001/27701/SOC 2) · WP-8.3 Government pack v1 (FIPS 140-3 build, STIG profile, NIST 800-171/CMMC mapping + SSP template, CUI labels, air-gap mode).
- **Migration Factory wave 2 (docs/16):** WP-7.5 NetSuite + Dynamics BC packs + parallel-run agent · WP-7.6 SAP + IFS packs + customization-inventory agent · WP-7.7 assessment mode + Notes/Domino pack (absorbs WP-5.5).
- **Portability & topology track:** WP-9.2 SQL Server adapter · WP-9.3 MySQL/MariaDB adapter (ADR-015; WP-9.1 adapter contract lands Phase 2, WP-9.4 Oracle/Db2 + WP-9.5 read-side sinks post-1.0/Phase 6) · WP-10.2 Patroni/CloudNativePG reference configs + failover chaos tests (docs/22; WP-10.1 topology bundles + `lasterp doctor` land Phase 4, WP-10.3 region-DR drills Phase 6) · Valkey cache adapter with WP-5.1 (ADR-016).
- **Milestone M5: 1.0** — public launch quality.

## Phase 6 — Self-evolution (see [13-SELF-EVOLUTION.md](13-SELF-EVOLUTION.md), ADR-014)
- WP-6.1 Learning substrate (L0) + suggestions (L1) · WP-6.2 Agent customization pipeline (L2) · WP-6.3 Shadow tenants · WP-6.4 Plugin-generation pipeline (L3) · WP-6.5 Self-healing runtime v1 · WP-6.6 Improvement-proposal loop (L4) · WP-6.7 Mutation testing + integrity-sentinel productization (docs/19).
- Note: WP-6.1/6.5 foundations (telemetry hooks, projection-rebuild machinery, circuit breakers) are built incrementally from Phase 2 onward; Phase 6 completes and productizes them.
- **Milestone M6: "the system that improves itself — inside the fence"**

## Standing tracks (every phase)
Security review per WP touching authz/sync/plugins · docs with every WP (user + dev) · performance budgets in CI · dogfood tenant runs the project's own books from M1 onward.
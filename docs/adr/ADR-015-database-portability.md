# ADR-015: Database portability — recommend Postgres, support what companies have

**Status:** Accepted · 2026-07-07 (amends ADR-002) · Requirement: Dan — "if a company wants to use something they already have, they should use it."

## Decision
The storage adapter (already the abstraction since ADR-002) becomes a **public, tiered portability contract**. Every supported engine must pass the full adapter conformance suite AND the Integrity Gauntlet (docs/19) — an engine that can't uphold the invariants doesn't ship, period.

| Tier | Engines | Support level |
|---|---|---|
| **1 — Recommended** | **PostgreSQL 16+** (flagship), **SQLite** (solo mode + client replicas) | Full features, first CI target, perf budgets guaranteed, hosted service runs it |
| **2 — Supported** | **Microsoft SQL Server 2019+**, **MySQL 8.4+ / MariaDB 11+** | Full core features via adapter; CI-gated conformance; noted degradations (below) |
| **3 — Enterprise adapters** | **Oracle 19c+**, **IBM Db2 12+** | Same conformance bar; maintained with partners/community; released when the suite is green, not before |
| **Not a system of record** | MongoDB, Cassandra | See below — supported as *read-side targets*, refused as the ledger's home |

### Per-engine notes (honesty in the matrix)
- **Tenant isolation:** Postgres RLS and SQL Server Row-Level Security map directly (INV-T1 backstop intact). MySQL/MariaDB/Oracle*/Db2 lack equivalent-quality RLS → the adapter enforces tenant predicates in the generated query layer (never hand-written SQL — the metadata engine emits it) **plus** we recommend database-per-tenant on these engines for regulated use. The Integrity Gauntlet's cross-tenant adversarial suite runs on every engine; passing it is the ship gate. (*Oracle VPD can serve where licensed.)
- **Feature degradation on non-Postgres:** semantic search (pgvector) falls back to engine-native FTS or an external embedding index; logical-replication change-feed tail falls back to the adapter's outbox-polling implementation (higher feed latency: seconds, not sub-second); JSONB custom-field indexing uses each engine's JSON facilities (all Tier-2/3 engines have adequate support).
- **The event store is deliberately lowest-common-denominator SQL** (append-only table + unique constraints + serializable/locked append) — it ports cleanly; this is why selective event sourcing (ADR-003) ages well here.

### MongoDB & Cassandra: the honest "no, but"
A double-entry ledger requires multi-row ACID transactions, uniqueness guarantees, and relational reporting as its native shape. Document/wide-column stores make the Integrity Gauntlet's guarantees either unprovable or rebuilt-by-hand on top (at which point you've written a worse Postgres). **Refusing this is a data-integrity decision (docs/19), not a preference.** What we offer instead: **materialized read-side sinks** — the change feed (docs/04) can continuously project into MongoDB/Cassandra/Elasticsearch/analytics stores for teams whose tooling lives there. Their data, their store, full fidelity — but the referee stays transactional.

## Rationale
- "Use what you already run" removes a real adoption blocker (DBA teams, licensing sunk costs, ops muscle memory — especially MSSQL shops).
- The metadata engine already generates all DDL/queries, so dialect support is concentrated in one layer — this is tractable *because* we never hand-write SQL.
- Tiering keeps the promise honest: every "supported" claim is backed by the full conformance + integrity suite in CI, not a README bullet.

## Consequences
- CI cost grows: Tier 1 runs on every PR; Tier 2 on merge-to-main + nightly; Tier 3 nightly. An engine that stays red for 30 days gets publicly demoted.
- Adapter contract is versioned and documented (docs/15 pattern) so the community can bring engines (e.g., CockroachDB, YugabyteDB) through the same gauntlet.
- Scale-out guidance is per-engine: Citus path is Postgres-only; MSSQL/Oracle/Db2 shops scale via their native HA/partitioning + DB-per-tenant (docs/22).
- Build plan: **WP-9.1** adapter contract v1 + conformance suite public (Phase 2, extracts from WP-0.2) · **WP-9.2** SQL Server adapter (Phase 5) · **WP-9.3** MySQL/MariaDB adapter (Phase 5/6) · **WP-9.4** Oracle + Db2 (post-1.0, partner-gated) · **WP-9.5** read-side sink framework (Mongo/Cassandra/ES materializers, Phase 6).

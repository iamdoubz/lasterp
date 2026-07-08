# ADR-016: Caching — layered by architecture, Valkey optional at scale

**Status:** Accepted · 2026-07-07 (the ADR that ADR-011's "no new runtime dependencies without an ADR" rule demanded)

## Context
Dan asked: do we cache, would Redis/Valkey help? Analysis: LastERP's architecture already *is* a cache hierarchy — most systems bolt on Redis to compensate for architecture LastERP doesn't have. But at the large/enterprise tiers, specific cross-node coordination problems appear that a shared cache solves cleanly. 2026 landscape: Valkey (Linux Foundation fork, BSD) has won the default slot — 100M+ Docker pulls, Snap/Pinterest migrations, ~20% cheaper managed, performance parity-or-better; Redis retains an edge only in features we don't need from it (its vector search — we have pgvector).

## Decision
Four cache layers; only the fourth is optional infrastructure:

1. **Client replica (the ultimate cache):** every read a user makes hits local SQLite — zero-latency, offline-capable, invalidated by the sync feed. This is why LastERP doesn't need a cache to be fast.
2. **Projections (precomputed answers):** balances, agings, dashboards read from purpose-built tables — "caching" with correctness guarantees (rebuildable, INV-E5-verified) instead of TTL guesswork.
3. **In-process node cache** (`kernel/cache`, ristretto-style): hot metadata schemas, permission matrices, FX/tax tables, idempotency fast-path — invalidated cluster-wide via NATS broadcast (already present). Free, no ops.
4. **Shared cache adapter — Valkey (optional, large/enterprise tiers only):** behind the same `kernel/cache` interface, enabled by config for the problems that are genuinely cross-node: distributed rate-limit counters (atomic, per-token/tenant), web-session/device-token revocation checks at high fan-out, cross-node idempotency-key check for very high write rates, dashboard fragment cache for 1000-viewer TV-mode scenarios. Redis-protocol compatible (works with ElastiCache/Memorystore Valkey, or Redis if a customer insists — "use what you have" applies here too).

**Hard rule (integrity):** the cache is never authoritative. Nothing financial is served from layer 3/4 without a version check; a cold or wiped cache changes latency, never answers. Cache poisoning/staleness scenarios are in the Integrity Gauntlet.

## Rejected
- Mandatory Redis/Valkey for all deployments: taxes every self-hoster with ops burden the architecture doesn't need (violates ADR-011's minimal-footprint principle).
- Redis (the company's editions) as default: license churn risk; Valkey is the neutral choice with identical ops surface.

## Consequences
- `kernel/cache` interface from Phase 0 (in-process impl); Valkey backend lands with WP-5.1 load hardening, where the nightly 50k test must show the specific wins (rate-limit accuracy across nodes, p99 under session-check load) to justify each use.
- Solo/team/medium topologies (docs/22) run zero external cache — permanently supported.

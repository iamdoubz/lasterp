# ADR-005: Multitenancy — shared schema + RLS, DB-per-tenant tier

**Status:** Accepted · 2026-07-06

## Context
Must serve a 5-user shop and a 50k-user enterprise from the same codebase; regulated tenants need hard isolation. 2026 consensus: shared schema + tenant_id + RLS is the right default; schema-per-tenant degrades pg_catalog beyond ~5k schemas and makes N-schema migrations an operational nightmare.

## Decision
- **Default:** every table carries `tenant_id`; Postgres **Row-Level Security** policies filter on `current_setting('app.tenant_id')`, set per-connection/transaction by kernel middleware. Isolation enforced by the database even if application code has bugs.
- **Enterprise/regulated tier:** dedicated database per tenant (same schema, same binaries, different DSN). Also the escape hatch for mega-tenants.
- **Never schema-per-tenant.**
- Solo mode (single tenant, SQLite) bypasses RLS; tenant_id still present so promotion to hosted is a data copy, not a migration.

## Rationale
- RLS overhead measured at 1–5% for typical queries — cheap insurance against catastrophic cross-tenant leaks.
- Shared schema = one migration, one connection pool, easy noisy-neighbor monitoring, Citus-shardable by tenant_id later.

## Consequences
- Kernel-level invariant tests: every request path must set tenant context; CI includes a test that queries without context and asserts zero rows.
- All indexes lead with `tenant_id`.
- Per-tenant rate limits and statement timeouts to contain noisy neighbors.

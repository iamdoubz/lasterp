# 09 — Scalability & Deployment

Target: 5 users on a laptop → 50,000 concurrent users, zero data loss, one codebase. Shapes defined in [ADR-011](adr/ADR-011-deployment.md); **sized topologies with failover/LB recommendations per company size in [22-DEPLOYMENT-TOPOLOGIES.md](22-DEPLOYMENT-TOPOLOGIES.md)**; database options per [ADR-015](adr/ADR-015-database-portability.md), caching per [ADR-016](adr/ADR-016-caching.md).

## Performance budgets (enforced in CI, k6 + custom harness)
| Metric | Budget |
|---|---|
| Interactive read (list/form, warm) | p95 < 100ms server; < 30ms from local replica |
| Write (command → committed) | p95 < 300ms |
| Sync catch-up (8h offline, typical user) | < 10s |
| Cold client hydration (typical scope) | < 60s |
| Projection lag | p99 < 1s |
| Server per-node capacity | ≥ 5,000 concurrent sync connections/node (validated by load test) |

## Scaling the tiers

**App tier (stateless):** N Go replicas; sync connections are cheap goroutines; HPA on CPU + connection count. Session state lives in tokens + DB, so any node serves any request. 50k concurrent ≈ 10–15 nodes with headroom — boring by design.

**Postgres (the real work):**
1. Right-size + pgbouncer + `tenant_id`-led indexes (most deployments stop here).
2. Read replicas: reports/analytics/hydration snapshots routed to replicas via query-class routing.
3. Native partitioning of `events` and large projections by tenant_id hash + time.
4. Citus: distribute by tenant_id — every tenant's data colocated on one shard, cross-tenant queries only in admin plane.
5. Mega-tenant / regulated → dedicated DB (also the compliance tier).
Change feed fan-out: sync engine tails the event log via logical replication slot into NATS; sync nodes subscribe — Postgres never fans out to 50k connections itself.

**NATS JetStream:** clustered ×3; subjects partitioned per tenant hash; consumers (projectors, webhooks, plugin async, connectors) horizontally scalable via queue groups.

**Zero data loss:** synchronous_commit=on (default), Postgres streaming replication with sync replica for cluster shape (RPO 0), `lasterp backup` = pgBackRest/wal-g + object-storage manifest; restore drill documented and CI-tested against a scratch instance monthly.

## Upgrades
- Expand → migrate → contract schema migrations only; app N and N+1 must both run against the transitional schema (zero-downtime rolling deploys).
- Metadata/customization compatibility check runs pre-upgrade with a human-readable report (ADR-006).
- Client protocol versioned; server supports current + previous minor.

## Observability (all shapes, on by default)
Prometheus metrics (per-tenant labels where cheap), OTel traces across gateway → command → eventstore → projector, structured logs with command_id correlation, built-in admin dashboards: sync lag per client fleet, projection lag, plugin resource use, connector health, slow queries. Self-hosters get a bundled Grafana dashboard set.

## CI performance gates
Every merge to main runs: unit/integration, sync simulation suite, k6 load smoke (budget table above at small scale), and a nightly full-scale load test (50k simulated sync clients against a staging cluster). A budget regression blocks release, not merge.

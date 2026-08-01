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

## Database roles (required on Postgres — WP-1.10)

LastERP deployments use **two** database roles, and the difference is load-bearing rather than hygiene:

| Role | Used by | Holds |
|---|---|---|
| owner | `lasterp migrate`, `lasterp harden` | schema ownership, and the `SECURITY DEFINER` pipeline functions (`append_event`, `ledger_post_entry`) |
| application | `lasterp serve` | everything needed to run, minus direct `INSERT` on `events` and any mutation of the append-only tables |

That second row is what makes INV-F5 — "no direct ledger writes outside the posting pipeline" — a *storage* guarantee instead of a convention the application is trusted to keep (docs/19 §2 layer 3). Serving as the owner is a supported-but-degraded posture, not an error, so nothing will stop you; `lasterp doctor` is what tells you which one you are in.

```sh
export LASTERP_DSN='postgres://owner:…@db/lasterp'      # owner connection
lasterp migrate                                          # schema, as the owner
LASTERP_APP_PASSWORD=… lasterp harden --create-role      # create + lock down lasterp_app

export LASTERP_DSN='postgres://lasterp_app:…@db/lasterp' # restricted connection
lasterp doctor                                           # exits non-zero if not separated
lasterp serve
```

Both `migrate` and `harden` are idempotent, so they belong in every deploy rather than a first-run runbook — an upgrade that adds a protected table needs the grants re-applied, and a grant nobody re-applied is a hole nobody notices. [deploy/docker-compose.yml](../deploy/docker-compose.yml) and the chart's pre-upgrade hook both do exactly the sequence above.

`doctor` exits non-zero when the deployment is not separated, so it works as a deploy gate rather than something a human has to read. It reports posture by reading the catalog (`has_table_privilege`), never by attempting a write that ought to fail.

**SQLite/solo mode has no roles** and is a single trusted process (ADR-005); `harden` is a no-op there and `doctor` says so rather than implying a separation that does not exist.

## Upgrades
- Expand → migrate → contract schema migrations only; app N and N+1 must both run against the transitional schema (zero-downtime rolling deploys).
- **Run `lasterp migrate` then `lasterp harden` as the owner before rolling the new version**, and keep serving as the application role (see above).
- Metadata/customization compatibility check runs pre-upgrade with a human-readable report (ADR-006).
- Client protocol versioned; server supports current + previous minor.

## Observability (all shapes, on by default)
Prometheus metrics (per-tenant labels where cheap), OTel traces across gateway → command → eventstore → projector, structured logs with command_id correlation, built-in admin dashboards: sync lag per client fleet, projection lag, plugin resource use, connector health, slow queries. Self-hosters get a bundled Grafana dashboard set.

## CI performance gates
Every merge to main runs: unit/integration, sync simulation suite, k6 load smoke (budget table above at small scale), and a nightly full-scale load test (50k simulated sync clients against a staging cluster). A budget regression blocks release, not merge.

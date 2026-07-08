# 22 — Deployment Topologies: failover, load balancing & sizing (5 → 50,000 users)

**Founding requirement (Dan, 2026-07-07):** if 50,000 users hammer the server, distribute the load; if a node goes down, survive it. Concrete recommendations per company size. Extends docs/09; DB choice per [ADR-015](adr/ADR-015-database-portability.md), cache per [ADR-016](adr/ADR-016-caching.md).

## Why load balancing is easy here (the architectural head start)

- **App nodes are stateless** (docs/01): session state lives in tokens + DB, so *any* node serves *any* request — a load balancer needs zero session affinity.
- **Node death is a non-event for clients:** sync connections carry cursors (docs/04); a client that loses its node reconnects to another and resumes from its cursor mid-stream. Offline-first means even total server loss = "working offline" — users keep working, sync resumes later. Almost no competitor can say this.
- **In-flight writes are safe:** a command either committed (client learns on reconnect via command_id dedupe, INV-E4) or it didn't (client's outbox resubmits). A dying node can never half-apply — transactions + the gauntlet guarantee it.

## The five topologies

### 1. Small — <100 users ("the office server")
- **Shape:** 1× `lasterp` binary + PostgreSQL on one box (or SQLite solo mode below ~25 users). 4–8 vCPU / 8–16GB. Embedded NATS, local/S3 files, no cache, no LB.
- **HA:** systemd auto-restart (seconds of blip; clients don't notice — offline mode absorbs it). Nightly verified backups off-box + WAL archiving (`lasterp backup`); documented restore = the DR plan. Optional warm standby via WAL shipping for the cautious.
- **Honest RTO/RPO:** RTO minutes–hours (restore), RPO ≈ backup/WAL-archive interval. Right trade for this size; a 20-person firm doesn't staff a failover cluster.

### 2. Medium — 100–500 users
- **Shape:** 2× app nodes behind an L4/L7 LB (Caddy/HAProxy/nginx or cloud LB) + 1× Postgres primary with **streaming replica** + pgbouncer. Embedded NATS per node → small NATS cluster when webhook/automation volume grows.
- **Failover:** app node dies → LB health check (`/readyz`) ejects it, zero user impact. DB primary dies → promote replica (scripted or Patroni); minutes of write pause, reads/offline unaffected.
- **RTO minutes / RPO ~0** (streaming replication; synchronous mode optional).

### 3. Medium-large — 500–2,000 users
- **Shape:** 3× app nodes + LB (rolling deploys become zero-downtime by default) + **Patroni-managed Postgres** (primary + 2 replicas, etcd consensus, automatic failover ~10–30s) or a managed HA Postgres (RDS Multi-AZ, Cloud SQL HA). NATS 3-node cluster. Reports/hydration routed to read replicas.
- This is the tier where **automatic** DB failover replaces scripted promotion — the ops team stops being the failover mechanism.
- **RTO <1 min (automatic) / RPO 0** with one synchronous replica.

### 4. Large — 2,000–10,000 users
- **Shape:** Kubernetes (or equivalent): 4–8 app replicas, HPA on CPU + sync-connection count, PodDisruptionBudgets, multi-AZ node spread. Postgres via **CloudNativePG operator** (declarative HA, native streaming replication, no external failover tooling) or managed equivalent; 1 sync + N async replicas; pgbouncer sidecar/pool. NATS JetStream 3-node cluster (queue groups scale projectors/webhooks horizontally). **Valkey enters here** (ADR-016): distributed rate limits, session revocation fan-out. Partitioning of events/large projections by tenant hash lands here too.
- **Failover:** AZ loss = degraded capacity, not outage. Rolling upgrades + expand/contract migrations = zero downtime.
- **RTO seconds–1min / RPO 0.**

### 5. Enterprise — 10,000+ users (validated at 50k concurrent)
- **Shape:** 10–15+ app replicas multi-AZ (the nightly 50k-client load test in CI keeps this honest, docs/09); L7 LB/ingress with connection draining; change-feed fan-out via NATS so Postgres never serves 50k subscriptions directly (docs/09). Postgres: CloudNativePG/Patroni with synchronous replica (RPO 0) + read-replica pool; **Citus** sharding by tenant_id or DB-per-mega-tenant when a single primary saturates (ADR-015: Citus is the Postgres path; MSSQL/Oracle shops use native partitioning + DB-per-tenant). Valkey 3-node. Object storage + CDN for attachments.
- **DR:** second region with async replication + object-storage replication; documented failover runbook, **drilled quarterly** (the drill is the deliverable — an untested DR plan is a rumor, docs/13). Cross-region RPO seconds, RTO <1h; in-region RPO 0/RTO seconds.
- 50k concurrent ≈ 3,500–5,000 sync connections/node at the validated ≥5k/node budget — comfortable headroom on ~12 nodes.

## Summary table

| Tier | Users | App | LB | Database | Queue | Cache | RPO / RTO |
|---|---|---|---|---|---|---|---|
| Small | <100 | 1 binary | — | 1× PG (or SQLite) + backups | embedded | — | backup-interval / min–hrs |
| Medium | 100–500 | 2 nodes | yes | PG + streaming replica | embedded→cluster | — | ~0 / minutes |
| Med-large | 500–2k | 3 nodes | yes | Patroni or managed HA (auto-failover) | NATS ×3 | — | 0 / <1 min |
| Large | 2k–10k | 4–8, HPA, multi-AZ | k8s ingress | CloudNativePG + replicas, partitioning | NATS ×3 | Valkey | 0 / seconds |
| Enterprise | 10k+ (50k conc.) | 10–15+, multi-AZ | L7 + draining | + sync replica, Citus/DB-per-tenant, region DR | NATS ×3+ | Valkey ×3 | 0 / seconds (region: sec / <1h) |

Every tier ships as a maintained reference: docker-compose (small/medium), Ansible/compose bundle (med-large), Helm values profiles (large/enterprise) — in `deploy/topologies/`, smoke-tested in CI. `lasterp doctor` reports which tier your deployment matches and what's missing for the next one.

## Build plan
- **WP-10.1** Topology reference bundles + `lasterp doctor` sizing report (Phase 4). AC: each bundle boots + passes smoke in CI; kill-a-node test shows client resume-from-cursor.
- **WP-10.2** Patroni/CloudNativePG reference configs + automatic-failover chaos tests in gauntlet (Phase 5, with WP-5.1). AC: DB failover under write load loses zero acknowledged writes (INV-S1).
- **WP-10.3** Region-DR runbook + drill automation (Phase 6). AC: scripted region failover on staging meets RTO/RPO targets.

# ADR-011: Deployment — single binary to Kubernetes, one codebase

**Status:** Accepted · 2026-07-06

## Decision
Three supported shapes, selected by config, identical application code:

1. **Solo:** `./lasterp serve` — embedded SQLite, embedded NATS, local file storage, built-in TLS (Let's Encrypt) or behind a reverse proxy. Target: 1–25 users on a $10 VPS/laptop/NAS. Install in <10 min.
2. **Team:** same binary + external Postgres (docker-compose file provided). 25–500 users.
3. **Cluster:** Helm chart — N stateless app replicas behind LB, Postgres (operator or managed), NATS cluster, S3-compatible storage. HPA on CPU + sync-connection count. 500–50k+ users.

Operational requirements baked in: `/healthz` + `/readyz`, Prometheus metrics, OpenTelemetry traces, structured JSON logs, one-command backup (`lasterp backup` = DB dump + object storage manifest), documented restore drill, zero-downtime rolling upgrades with expand→migrate→contract DB migrations.

## Rationale
- Self-hostability is a product feature, not an ops afterthought; every external dependency added to the minimum footprint must be fought for.
- Stateless app tier + Postgres + NATS is a boring, well-trodden 50k-user shape.

## Consequences
- No feature may require infra outside the shape table (e.g., Redis, Elastic) without an ADR.
- CI builds and smoke-tests all three shapes on every merge to main.

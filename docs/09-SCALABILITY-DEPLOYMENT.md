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

## Secrets key file (required before the vault can store anything — WP-3.0)

The secrets vault (docs/08 §Data protection) seals every stored credential under a per-secret data key, and wraps that data key with a **key-encryption key the deployment owns**. `LASTERP_SECRETS_KEYFILE` names the file holding it:

```sh
lasterp secrets init -keyfile /etc/lasterp/lasterp.keys   # one fresh key, mode 0600, never overwrites
export LASTERP_SECRETS_KEYFILE=/etc/lasterp/lasterp.keys
lasterp serve
```

**Back that file up, and keep it out of the database backup.** It is the one piece of a LastERP deployment with no recovery path: the vault's ciphertext is worthless without it, which is the point, and it is equally worthless *to you*. Nothing generates a key file implicitly — a silently created KEK is discovered at restore time by someone holding a database backup and no key ([WP-3.0-decisions.md](notes/WP-3.0-decisions.md) §3).

With no key file the server still starts and the vault's routes still answer: storing a secret returns `503` with `type: "secrets-no-key-source"` naming the variable to set, and listing still works so an operator can see what a lost key file cost them.

Rotating the KEK — add the new key, point `current` at it, re-wrap, and only then remove the old one:

```sh
# edit the key file: add `2026-09-a = <base64 32 bytes>`, set `current = 2026-09-a`
lasterp secrets rotate -tenant acme          # re-wraps that tenant's data keys
# the old key stays readable until every row has moved; remove it last
```

Rotation re-wraps the data keys and never touches the payloads, so it is cheap and re-runnable; a crashed run is resumed by running it again. One tenant per invocation.

## Plugin outbound HTTP (WP-3.2a)

A plugin reaches the network only through `lasterp_http_request`, only to the hosts its manifest declared and an administrator approved, and every call writes an `audit_log` row naming the plugin, the method and the destination (no headers, no bodies — a plugin's bearer token must not become the longest-lived copy of that credential). Redirects are not followed and the scheme is always `https`.

**By default a plugin cannot reach a private address**, even one its manifest names: the check runs on the address actually being dialled, so an allowlisted DNS name that resolves to `169.254.169.254`, `10.0.0.0/8` or `127.0.0.1` is refused at the socket. That is the SSRF case — the cloud metadata service and every internal service that trusts its own LAN sit behind exactly that name resolution.

A self-hoster whose plugin legitimately calls an internal service turns it on for the whole deployment:

```sh
export LASTERP_PLUGIN_HTTP_ALLOW_PRIVATE=1   # plugins may dial RFC1918/loopback destinations
```

It is a deployment setting rather than a manifest capability on purpose: "may plugins reach this network" is a fact about where LastERP runs, and a plugin asking for it would be the plugin deciding. Leave it unset unless a plugin needs it, and prefer narrowing the manifest allowlist to the one internal host.

A plugin calling an internal service usually meets a certificate from a company CA rather than a public one. Name that CA's PEM file and it is added to the system roots for plugin outbound calls only:

```sh
export LASTERP_PLUGIN_HTTP_CA_FILE=/etc/lasterp/internal-ca.pem
```

There is no "skip verification" knob, and there will not be one.

## Plugin publisher trust (WP-3.2b)

A plugin can be installed two ways. `POST /api/v1/plugins` takes a module an administrator already has and vouches for. **`POST /api/v1/plugins/bundle` takes a signed bundle**, and installs it only if a publisher this deployment trusts signed it — which is what `lasterp plugin install` uses.

Trust is a file of publisher keys, in the same shape as the vault's key file:

```sh
cat /etc/lasterp/plugin-publishers
# acme-2026 = kR3f…base64 ed25519 public key…
# ops-internal = 9vQ2…
export LASTERP_PLUGIN_TRUST_FILE=/etc/lasterp/plugin-publishers
```

A publisher generates their half with `lasterp plugin keygen`, which prints exactly the line to paste here. With the variable unset the file is simply empty, and **every bundle is refused** — a deployment that installs modules directly is unaffected, and one that meant to configure trust finds out from the refusal rather than from a bundle installing unverified. A *malformed* trust file fails the boot on purpose: a security control that quietly degrades to "trust nothing" is one nobody notices until they need it.

Trust is deployment-wide rather than per-tenant, deliberately ([WP-3.2-decisions.md](notes/WP-3.2-decisions.md) §3): every install today is an operator handing a bundle to their own deployment, so "who may publish plugins here" is an operator fact like the key-encryption key. Per-tenant publisher lists arrive with the tenant-facing registry that needs them.

## Upgrades
- Expand → migrate → contract schema migrations only; app N and N+1 must both run against the transitional schema (zero-downtime rolling deploys).
- **Run `lasterp migrate` then `lasterp harden` as the owner before rolling the new version**, and keep serving as the application role (see above).
- Metadata/customization compatibility check runs pre-upgrade with a human-readable report (ADR-006).
- Client protocol versioned; server supports current + previous minor.

## Observability (all shapes, on by default)
Prometheus metrics (per-tenant labels where cheap), OTel traces across gateway → command → eventstore → projector, structured logs with command_id correlation, built-in admin dashboards: sync lag per client fleet, projection lag, plugin resource use, connector health, slow queries. Self-hosters get a bundled Grafana dashboard set.

## CI performance gates
Every merge to main runs: unit/integration, sync simulation suite, k6 load smoke (budget table above at small scale), and a nightly full-scale load test (50k simulated sync clients against a staging cluster). A budget regression blocks release, not merge.

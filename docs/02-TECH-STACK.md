# 02 — Tech Stack

Full rationale in [docs/adr/](adr/). Summary:

| Layer | Choice | Why (short) | ADR |
|---|---|---|---|
| Server language | **Go 1.26.4** (pinned; track latest stable patch) | Single static binary (self-hosting), goroutines handle 50k+ conns, fast compile = fast agent iteration, huge ecosystem, easy to write correctly | [ADR-001](adr/ADR-001-server-language.md) |
| Hot paths (later) | Rust via WASM/FFI | Only if profiling demands it | ADR-001 |
| Server DB | **PostgreSQL 18+ recommended**; supported: SQL Server, MySQL/MariaDB (Tier 2), Oracle, Db2 (Tier 3); Mongo/Cassandra as read-side sinks only | Use what you have — every engine passes the same conformance + integrity suite | [ADR-002](adr/ADR-002-database.md), [ADR-015](adr/ADR-015-database-portability.md) |
| Solo-mode DB | **SQLite** (embedded) | Zero-dependency self-hosting | ADR-002 |
| Cache | Client replica + projections + in-process by default; **Valkey adapter** optional at large/enterprise tiers | The architecture is the cache; Valkey only for cross-node coordination | [ADR-016](adr/ADR-016-caching.md) |
| HA / failover | Tiered topologies: systemd restart → streaming replica → Patroni/CloudNativePG auto-failover → Citus + region DR | Sized recommendations from <100 to 50k users | [docs/22](../docs/22-DEPLOYMENT-TOPOLOGIES.md) |
| Client store | **SQLite** (WASM in browser via OPFS; native in Tauri/mobile) | Real local queries, proven offline tech | [ADR-004](adr/ADR-004-sync-model.md) |
| Persistence pattern | **CQRS + selective event sourcing** | Ledger = append-only events; master data = rows + audit | [ADR-003](adr/ADR-003-event-sourcing.md) |
| Sync | **Server-authoritative command replay** (Lotus-Notes-style replicas, modern referee) | Ledgers can't merge via CRDT; server validates everything | ADR-004 |
| Multitenancy | **Shared schema + tenant_id + Postgres RLS**; DB-per-tenant tier for regulated/enterprise | Industry consensus default; schema-per-tenant is a known trap | [ADR-005](adr/ADR-005-multitenancy.md) |
| Customization | **Metadata objects + overlays** | Upgrades never conflict with customizations | [ADR-006](adr/ADR-006-metadata-customization.md) |
| Plugins | **WASM via Extism** (server), sandboxed ES modules (UI) | Any language, true sandbox, capability permissions | [ADR-007](adr/ADR-007-plugin-system.md) |
| AI | **MCP-native**, agents as principals, pgvector embeddings | AI through same APIs/permissions/audit as humans | [ADR-008](adr/ADR-008-ai-first.md) |
| APIs | REST/JSON (public) + gRPC (internal/sync) + webhooks + MCP | Everything-is-an-API | [ADR-009](adr/ADR-009-api-strategy.md) |
| Async/events | **NATS JetStream** (embedded lib in solo mode) | Jobs, webhooks, change feed; embeds in Go binary | ADR-009 |
| Frontend | **React + TypeScript + Vite**, TanStack Query/Router, shadcn-style components | Talent pool, agent familiarity, ecosystem | [ADR-010](adr/ADR-010-frontend.md) |
| Desktop | Tauri | Native SQLite, small footprint | ADR-010 |
| Search | Postgres FTS + pgvector (semantic) | No extra infra until proven necessary | ADR-002 |
| Deploy | Docker Compose + Helm chart; **single binary** for solo | 10-minute self-host is a hard requirement | [ADR-011](adr/ADR-011-deployment.md) |
| License | **AGPLv3 core, Apache-2.0 SDKs/client libs** (decided 2026-07-07) | Prevent closed-cloud capture, keep plugin ecosystem friction-free | [ADR-012](adr/ADR-012-license.md) |
| Tax & FX data | Adapter pattern: open sources (ECB FX) built-in; Avalara/Vertex/PayrollTax API as connector plugins; local editable tax tables always available | No hard dependency on any commercial data vendor | [ADR-013](adr/ADR-013-reference-data.md) |

## Repository layout (planned)

```
lasterp/
├── CLAUDE.md
├── docs/                    # this documentation
├── kernel/                  # Go: metadata engine, events, authz, sync, plugin host
│   ├── metadata/
│   ├── eventstore/
│   ├── authz/
│   ├── sync/
│   ├── plugins/
│   └── api/
├── modules/                 # Go: business modules (each self-contained)
│   ├── ledger/
│   ├── invoicing/
│   ├── payables/
│   ├── crm/
│   ├── payroll/
│   └── inventory/
├── connectors/              # integration adapters (Salesforce, SAP CPQ, Outlook…)
├── web/                     # React/TS client + sync client + SQLite-WASM
├── clients/desktop/         # Tauri shell
├── sdk/                     # plugin PDKs (Rust, Go, TS, Python) + API client libs
├── deploy/                  # docker-compose, helm, systemd
└── tools/                   # codegen, migration tooling, seed data
```

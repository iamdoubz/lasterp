# 01 — System Architecture

## Overview

LastERP is a **modular monolith kernel** with **event-sourced financial core**, **metadata-driven object system**, **server-authoritative sync**, and **sandboxed extension points** — deployable as a single binary or a horizontally scaled cluster.

```
┌─────────────────────────────────────────────────────────────────┐
│ CLIENTS                                                         │
│  Web (React/TS + SQLite-WASM)   Desktop (Tauri)   Mobile        │
│  ── each holds a local SQLite replica + outbound mutation queue │
└──────────────────────┬──────────────────────────────────────────┘
                       │ Sync protocol (gRPC-web / WebSocket)
┌──────────────────────▼──────────────────────────────────────────┐
│ LASTERP SERVER (Go, stateless, N replicas)                      │
│                                                                 │
│  API Gateway: REST/JSON + gRPC + Webhooks + MCP server          │
│  ┌──────────┬───────────┬──────────┬──────────┬─────────────┐   │
│  │ Metadata │ Command   │ Query    │ Sync     │ Plugin Host │   │
│  │ Engine   │ Processor │ Engine   │ Engine   │ (WASM/      │   │
│  │(objects, │(validate, │(projec-  │(per-     │  Extism)    │   │
│  │ fields,  │ authorize,│ tions,   │ tenant   │             │   │
│  │ workflows│ append    │ reports, │ logs,    │  Connector  │   │
│  │ perms)   │ events)   │ search)  │ cursors) │  Framework  │   │
│  └──────────┴───────────┴──────────┴──────────┴─────────────┘   │
│  Kernel services: AuthN/Z · Audit · Jobs · Notifications · AI   │
└───────┬───────────────────┬─────────────────────┬───────────────┘
        │                   │                     │
┌───────▼───────┐   ┌───────▼────────┐   ┌────────▼────────┐
│ PostgreSQL    │   │ NATS JetStream │   │ Object storage  │
│ (events +     │   │ (async events, │   │ (S3/minio:      │
│  projections, │   │  job queue,    │   │  attachments,   │
│  RLS, pgvector│   │  webhooks out) │   │  exports)       │
│  full-text)   │   │  *embedded in  │   │  *local FS in   │
│  *SQLite in   │   │  single-binary │   │  single-binary  │
│  solo mode    │   │  mode          │   │  mode           │
└───────────────┘   └────────────────┘   └─────────────────┘
```

## Core architectural patterns

### 1. Modular monolith, not microservices
One Go binary containing well-bounded modules (ledger, invoicing, CRM, payroll…) communicating through in-process interfaces and domain events. Microservices at day one would kill velocity and self-hostability. Module boundaries are enforced by internal package visibility + a lint rule (no cross-module imports except via the module API). If a module ever needs independent scaling, the seams already exist.

### 2. CQRS + selective event sourcing
- **Financial domains (GL, AR, AP, payroll runs, inventory movements): event-sourced.** Append-only event streams per aggregate (e.g., `invoice:INV-0042`). State = fold(events). Corrections are compensating events (mirrors double-entry practice; gives audit and sync for free).
- **Master data (customers, items, employees, settings): conventional rows + mandatory audit log.** Event-sourcing everything is dogma; master data doesn't need replay semantics.
- **Projections:** async projectors fold events into read-optimized tables (balances, aging reports, dashboards). Projections are disposable — rebuildable from the event log.

### 3. Metadata object system ("Objects")
Every business entity — core or custom — is defined by a versioned schema document (like Frappe DocTypes, but typed, migration-aware, and horizontally scalable). The metadata engine generates: DB storage (typed columns for core fields, JSONB for custom), REST/gRPC APIs, validation, list/form UI descriptors, permissions, and MCP tool definitions. See [03-DATA-MODEL.md](03-DATA-MODEL.md).

### 4. Server-authoritative sync
Clients hold a SQLite replica of the data they're entitled to (shaped by "sync scopes"). Offline writes queue as **proposed commands**, not applied facts. Server replays them through full validation on reconnect. No CRDTs for business data — a ledger must have one referee. See [04-SYNC-ENGINE.md](04-SYNC-ENGINE.md).

### 5. Extension points, all sandboxed
- **Server plugins:** WASM (Extism) — any language, capability-scoped, resource-limited.
- **UI plugins:** ES modules loaded into defined slots, iframe-sandboxed for untrusted ones.
- **Connectors:** declarative + WASM transform hooks for third-party systems.
- **Automations:** user-defined workflows (trigger → condition → action) stored as metadata.

### 6. AI as first-class actor
Built-in MCP server exposes every module's operations as tools with the caller's permissions. Agent sessions are principals with roles, budgets, approval gates, and a dedicated audit trail. pgvector powers semantic search over all objects. See [06-AI-INTEGRATION.md](06-AI-INTEGRATION.md).

## Request lifecycles

**Write (online):** Client → API gateway → authorize (RBAC + RLS context) → command handler loads aggregate, validates business rules → append event(s) in Postgres tx (optimistic concurrency on stream version) → tx commits → projectors update read models → change feed notifies subscribed clients/webhooks/plugins.

**Write (offline):** Client applies command optimistically to local SQLite (marked `pending`) → queues in outbox → on reconnect, server replays commands in order → accepted: event appended, client marks confirmed; rejected: client rolls back local effect, surfaces conflict UI with server reason.

**Read:** served from projections (server) or local replica (client). Reports run against projection tables; heavy analytics can point at a read replica.

## Deployment shapes

| Shape | App | DB | Queue | Storage | Target |
|---|---|---|---|---|---|
| Solo | single binary | embedded SQLite | embedded | local FS | 1–25 users, laptop/VPS |
| Team | single binary | Postgres | embedded NATS | local FS/S3 | 25–500 users |
| Cluster | N replicas (k8s) | Postgres + replicas | NATS cluster | S3 | 500–50k+ |

Same code, config-selected adapters. See [09-SCALABILITY-DEPLOYMENT.md](09-SCALABILITY-DEPLOYMENT.md).

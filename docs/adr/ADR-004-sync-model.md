# ADR-004: Sync model — server-authoritative command replay over SQLite replicas

**Status:** Accepted · 2026-07-06

## Context
Requirement: totally offline operation with sync (Lotus Notes-grade reliability). Surveyed 2026 options: CRDTs (Yjs/Automerge), last-write-wins replication (ElectricSQL), one-way replication + write-back (PowerSync), server-authoritative mutations (Zero/LiveStore-style event sync).

## Decision
Build a **server-authoritative sync engine in the kernel** (not a third-party dependency):

1. **Downstream:** each client subscribes to *sync scopes* (tenant + role + module filters → the subset of data it's entitled to). Server streams ordered changes from the event log + master-data audit feed, identified by a per-scope monotonic cursor. Client folds them into its SQLite replica. Resumable from any cursor; full re-shape on scope change.
2. **Upstream:** offline writes are **commands in an outbox**, applied optimistically to the local replica and marked `pending`. On reconnect the server replays each command through the exact same validation/authorization path as online writes. Outcomes: **accept** (events appended, client confirms), **reject** (client rolls back, shows reason), **rebase** (server applies against newer state when commutative, e.g., two users appending different journal entries).
3. **Conflict UX:** rejected commands land in a "needs attention" tray with the server's reason and a one-click retry-after-edit. No silent data loss, ever.
4. **CRDTs only for prose:** collaborative note/description fields may use Yjs later. Never for business/financial data.

## Rationale
- **A ledger must have one referee.** LWW can silently drop a payment; CRDTs can merge two states into one that violates double-entry. Only server-side revalidation guarantees invariants.
- Commands-not-facts upstream means permissions, sequence numbering (invoice numbers!), and business rules are enforced in exactly one place.
- Third-party engines (ElectricSQL, PowerSync, Zero) are impressive but: LWW defaults, external service dependencies, or immature production stories — and none understands our command validation. The event log we already have (ADR-003) is 80% of the downstream engine.

## Rejected
- **CRDT-first:** wrong tool for invariant-rich data.
- **ElectricSQL/PowerSync as core dependency:** viable accelerators, but sync is our crown jewel; owning it avoids betting the company on a startup's roadmap. Revisit for mobile read-replicas if our timeline slips.

## Consequences
- Sequential identifiers (invoice numbers) are assigned **server-side at acceptance**, not offline. Offline documents carry a draft ID; UI communicates this clearly.
- Clients must handle "my write was rejected" as a first-class flow.
- Sync protocol (gRPC/WebSocket) is versioned from day one; old clients keep working.

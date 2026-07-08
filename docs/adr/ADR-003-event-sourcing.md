# ADR-003: Persistence pattern — CQRS with selective event sourcing

**Status:** Accepted · 2026-07-06

## Context
An ERP's financial core needs: immutable audit history, correction-by-reversal (accounting law in most jurisdictions), offline sync with deterministic conflict handling, and rebuildable reports. Event sourcing maps 1:1 onto double-entry bookkeeping (the original append-only ledger). But event-sourcing *everything* (settings, contact names) adds ceremony without benefit.

## Decision
- **Event-sourced domains:** general ledger, invoices, payments, AP bills, payroll runs, inventory movements, credit notes — anything that is or produces a financial fact.
  - Aggregates with append-only streams in a Postgres/SQLite `events` table: `(stream_id, version, tenant_id, type, payload jsonb, actor, occurred_at, command_id)`.
  - Optimistic concurrency: append fails if stream version moved; handler reloads, revalidates, retries.
  - Posted financial documents are never mutated. Corrections append compensating events (Tryton-style rigor, enforced at the storage layer).
- **CRUD domains:** master data (customers, items, employees, chart-of-accounts structure, settings) as ordinary rows with a mandatory, kernel-enforced audit log (old/new values, actor, timestamp).
- **Projections:** async projectors consume the event feed into read models (account balances, AR aging, stock levels). Projectors are idempotent and rebuildable; projection lag is monitored and bounded (<1s p99 target).

## Rationale
- The event log doubles as the **sync feed** (ADR-004) and the **audit trail** — three hard requirements, one mechanism.
- Deterministic replay makes offline command reconciliation tractable.
- "Reject, reorder, or keep" are the only operations on immutable events — vastly simpler conflict space than mutable-state merging.

## Rejected
- Full event sourcing everywhere: cost without benefit for master data.
- Plain CRUD + audit table for financials: audit-by-convention always rots; append-only-by-construction cannot.

## Consequences
- Snapshotting for long streams (e.g., high-volume stock items) from day one of the eventstore package.
- Event schema evolution discipline: events are versioned, upcasters migrate old shapes; events are never rewritten.

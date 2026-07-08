# ADR-010: Frontend — React + TypeScript, SQLite-WASM local store, Tauri desktop

**Status:** Accepted · 2026-07-06

## Decision
- **Web client:** React 19 + TypeScript + Vite. TanStack Router/Query/Table. Tailwind + shadcn-style component library ("LastERP UI Kit"). Forms/lists/detail views are **rendered from metadata UI descriptors** (ADR-006) with slot-based extension points (ADR-007); hand-built screens only where metadata rendering genuinely can't serve (dashboards, reconciliation workbench).
- **Local store:** SQLite compiled to WASM, persisted via OPFS; the sync client (ADR-004) maintains the replica and outbox. UI queries the local DB first — instant reads, offline by default.
- **Desktop:** Tauri wrapping the same web app with native SQLite (faster, no OPFS limits). **Mobile:** Tauri mobile or React Native shell, phase 4+; same sync client core (Rust or TS lib TBD by spike WP-2.6).
- Virtualized lists, keyboard-first interaction, sub-100ms perceived latency budget on all common screens.

## Rationale
- React/TS: deepest talent + agent familiarity + component ecosystem; the metadata-rendered UI keeps app code small anyway.
- Local SQLite replica is what makes "fast" and "offline" the same feature.

## Rejected
- Svelte/Solid (smaller ecosystems), native apps per platform (3× cost), server-rendered UI à la Frappe Desk (kills offline).

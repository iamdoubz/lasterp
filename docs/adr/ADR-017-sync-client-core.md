# ADR-017: Sync-client core — TypeScript, one core per shell, replica in a worker

**Status:** Accepted · 2026-08-02 · Spike: WP-2.6 ([decisions](../notes/WP-2.6-decisions.md))

## Context

[ADR-010](ADR-010-frontend.md) deferred this deliberately: *"Mobile: Tauri mobile or React
Native shell, phase 4+; same sync client core (Rust or TS lib TBD by spike WP-2.6)."* The
core in question is the replica apply loop, cursor bookkeeping, the outbox state machine and
conflict records — the piece three shells must share (browser over SQLite-WASM/OPFS, Tauri
desktop over native SQLite, mobile over native SQLite).

The UI language is not in scope; ADR-010 fixes React + TypeScript and this does not reopen
it. The question is only what the core is written in.

WP-2.6 set four falsifiable bars before building anything, so the answer could not be
reverse-engineered from whichever prototype happened to get built, and committed to
prototyping Rust/WASM in full if any bar failed.

## Decision

**The sync-client core is TypeScript**, one implementation shared by every shell, with the
platform's SQLite behind a small synchronous port (`web/src/sync/port.ts`).

**The replica runs in a dedicated worker.** This is not a TypeScript consequence — it falls
out of the platform and would bind a Rust core identically. See §Consequences.

## Evidence

Measured on the prototype in `web/src/sync/` (Chromium via Playwright for the browser
figures, Node 26 for the headless ones):

| Bar | Threshold | Measured | |
|---|---|---|---|
| 1. One core, two drivers, no platform branches | must hold | 6-case suite passes against `node:sqlite` **and** SQLite-WASM/OPFS from one `suite.ts`, zero conditionals in core | **pass** |
| 2. Apply throughput into OPFS | ≥ 5,000 changes/s | **44,651/s** (50,000 changes in 1,120 ms, batched 5,000/txn) | **pass** |
| 3. Core size | < 50 KB gz | **708 B gz** (1,566 B minified), excluding SQLite-WASM | **pass** |
| 4. INV-S properties testable headlessly | must hold | 7 vitest cases, no browser | **pass** |

Bars 2 and 3 pass by 8.9× and ~70×, which is worth saying plainly: **they were never the
constraint, and they are not the reason for this decision.** The core's work is
orchestration, not computation — cursor arithmetic and one `INSERT … ON CONFLICT` per
change — and the cost that matters is SQLite's, which both candidate languages pay
identically. Anyone citing performance as the reason to prefer either language here is
citing a number that measures SQLite.

The decision rests on bars 1 and 4, and on the type-sharing argument below.

## Rationale

1. **It is the language of the seam it plugs into.** `web/src/api/client.ts` was written in
   WP-1.5 with the note that "the sync client replaces the transport in this file, and no
   screen changes". A TS core swaps in behind that seam. A Rust core needs a JS shim there
   anyway, so the seam gets a second language without losing the first.
2. **Types come from metadata, and CLAUDE.md forbids duplicating them.** WP-2.2 generates
   replica schemas from the same metadata that already generates the client's TS types. A
   Rust core needs a second generator or a hand-maintained mirror of every object — the
   no-hand-written-duplicates rule broken in a different file, and a drift surface in the
   component whose entire job is not drifting.
3. **Every currently specified shell has a JS runtime.** Browser, Tauri webview, Tauri
   mobile, React Native — a TS core runs in all of them unmodified. This is a statement about
   today's plan, and §Revisit says what would change it.
4. **Headless simulability.** WP-2.3's harness must run N virtual clients with partitions and
   crashes. A core that is plain TS over a port runs N times in one Node process with no
   browser and no WASM; bar 4 is that property, demonstrated.

## Rejected: Rust compiled to WASM

Rejected on cost, not capability — it would work, and the reasons it loses are specific:

- **A second type generator**, per rationale 2. This is the strongest objection and it is
  structural, not a matter of effort.
- **Three binding surfaces to own** (`wasm-bindgen` for the browser, FFI for Tauri, JSI or
  equivalent for a React Native shell), each with its own serialization boundary at exactly
  the place invariants are enforced.
- **A second WASM module in the browser** alongside SQLite-WASM, with the replica's data
  crossing between them.

**The argument deliberately not used:** "Rust adds a toolchain." Tauri requires Rust
regardless (WP-4.7), so the marginal cost is Rust *business logic and bindings*, not the
toolchain. That cheap argument would have decided this by default and it is not sound.

**What Rust would have bought, and what is therefore given up:** the core is invariant-bearing
(INV-S1–S5), and exhaustive matching over a state machine with no implicit coercion is a
better host for that than TypeScript, whose guarantees end at every `any` and at each I/O
boundary. That is a real loss. It is mitigated — not erased — by strict mode, by the port
being the only I/O surface, and by the server remaining the referee (ADR-004): a client that
corrupts its own replica is a client that re-hydrates, not a ledger that loses money.

## Consequences

- **The replica lives in a dedicated worker, whichever language won.** Both OPFS VFSes rest
  on `FileSystemFileHandle.createSyncAccessHandle()`, which browsers expose only in a
  dedicated worker; on the main thread the SAH-pool installer fails with "Missing required
  OPFS APIs". WP-2.2 therefore designs a worker boundary — UI ⇄ worker messaging, and an
  async surface at that boundary even though the core is synchronous inside it.
- **Use the SAH-pool VFS, not the default `opfs` one.** The default needs `SharedArrayBuffer`
  and therefore COOP/COEP response headers. Cross-origin isolation would break third-party
  iframe embedding, which docs/05 §UI extension slots and WP-3.6 are built on. SAH-pool needs
  neither header. Had the spike reached for the default, the whole product would have paid a
  header tax to serve one subsystem.
- **The SAH pool pre-allocates a fixed number of file slots** (default 6) and fails with
  `SQLITE_CANTOPEN` beyond it rather than growing. WP-2.2 picks the capacity deliberately
  once it knows how many files a replica uses.
- **`optimizeDeps.exclude` for `@sqlite.org/sqlite-wasm`** in `vite.config.ts`: pre-bundling
  separates the JS from its sibling `.wasm`, and the fetch falls through to the SPA handler,
  so the runtime tries to instantiate `index.html` as WebAssembly.
- **WP-2.2** builds on `web/src/sync/` rather than starting fresh. **WP-4.7** (Tauri) supplies
  a native-SQLite adapter behind the same port; `node:sqlite` already proves the port admits a
  non-WASM driver. **WP-2.3**'s simulation harness drives the core headlessly.

## Revisit if

- **Mobile ships a native shell with no JS runtime.** This is the one condition that inverts
  the decision outright, and ADR-010 leaves that shell open. Decide it in the mobile WP, not
  before.
- **The core stops being orchestration.** If client-side computation over the replica (large
  local aggregation, a client-side report engine) becomes hot, re-measure — the performance
  bars above were slack precisely because no such workload exists today.
- **Replica corruption shows up in practice** in a way strict TypeScript did not prevent and
  a stronger type system would have. That is the honest failure mode of this decision, and it
  should be treated as evidence rather than explained away.

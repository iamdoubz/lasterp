# WP-2.6 decisions — shared sync-client core spike

Roadmap: *"Spike: shared sync-client core (TS lib vs Rust/WASM lib for web+Tauri+mobile).
Output: ADR-017."* Context: [ADR-010](../adr/ADR-010-frontend.md) §Desktop defers this
explicitly ("same sync client core (Rust or TS lib TBD by spike WP-2.6)"),
[ADR-004](../adr/ADR-004-sync-model.md), [docs/04](../04-SYNC-ENGINE.md).

This WP produces a decision, not a subsystem. What follows is how the decision gets made,
written before the work so the answer cannot be reverse-engineered from whichever prototype
happened to get built.

## 1. What is actually being chosen

Not "TypeScript or Rust for the client" — ADR-010 already fixes React + TypeScript for the
UI, and that is not reopened. The question is narrower: **what language owns the sync core**,
meaning the replica apply loop, cursor bookkeeping, the outbox state machine, and conflict
records. Everything above it (screens, routing, rendering) stays TS either way; everything
below it (SQLite) is C either way.

The core is the thing three shells must share:

| Target | Shell | Local SQLite | Status |
|---|---|---|---|
| Web | React in a browser | SQLite-WASM over OPFS | WP-2.2 |
| Desktop | Tauri webview | native SQLite via Rust | WP-4.7 |
| Mobile | Tauri mobile *or* React Native | native SQLite | Phase 4+, shell undecided |

## 2. The honest statement of the trade

**For TS:** it is already the language of the seam the core has to slot into
(`web/src/api/client.ts`, written in WP-1.5 explicitly so "the sync client replaces the
transport in this file"). It is the language of the metadata-generated types WP-2.2 consumes,
and CLAUDE.md forbids hand-writing duplicates of those — a Rust core needs a second
generator or a hand-maintained mirror, which is the same rule broken in a different file.
It runs unmodified in all three shells as they are currently specified, because every one of
them has a JS runtime.

**For Rust:** the core is invariant-bearing (INV-S1–S5), and a state machine with exhaustive
matching and no implicit coercions is a better host for that than TypeScript, whose
guarantees evaporate at any `any` and at every I/O boundary. If mobile ever becomes a native
shell without a webview, a Rust core is the only one that survives the move.

**The argument that does *not* hold, and should not be used:** "Rust adds a toolchain."
Tauri requires a Rust toolchain regardless (WP-4.7), so the marginal cost is not Rust
itself — it is owning Rust *business logic* plus `wasm-bindgen` plus three binding surfaces,
which is a real cost but a different and smaller one than it first appears. Stating this
because it is the cheap argument that would otherwise decide the question by default.

## 3. Falsifiable bars, fixed before measuring

A spike that prototypes only the favoured option is a rubber stamp. So the TS prototype is
built first (it is the cheaper one, and it seeds WP-2.2 if it wins) against bars set now.
**If any bar fails, Rust/WASM gets a full prototype before ADR-017 is written.**

1. **One core, two drivers, no conditionals.** The same core module passes the same test
   suite against SQLite-WASM/OPFS *and* against a native driver, with the driver behind a
   port and zero `if (platform)` branches in core code. This is the whole portability claim;
   if it needs branching at this seam it will need it at every later one.
2. **Apply throughput ≥ 5,000 changes/sec** into SQLite-WASM/OPFS in a batched transaction.
   The number comes from hydration, not steady state: a 100k-row replica must land in well
   under a minute or first-run offline is a bad experience. Steady-state feed volume is
   nowhere near this.
3. **Core adds < 50 KB gzipped**, excluding SQLite-WASM itself (~1 MB, which both options pay
   identically and which therefore decides nothing).
4. **The INV-S properties are expressible as automated tests** against the core with no
   browser in the loop, because WP-2.3's simulation harness has to drive N virtual clients
   headlessly and a core that only runs in a real browser cannot be simulated N times.

Bars 1 and 4 are the ones that matter. Bars 2 and 3 are sanity checks that would only fire
if something is badly wrong.

## 4. What the spike deliberately does not do

- **No Tauri shell.** WP-4.7 builds it. The native-driver half of bar 1 is served by Node's
  built-in `node:sqlite` (Node 26 is the pinned toolchain), which is a genuinely different
  driver with a genuinely different API surface — enough to prove the port, without standing
  up a desktop app to learn it.
- **No mobile anything.** The shell is undecided until Phase 4; the spike records what each
  option would cost when that choice arrives rather than pretending to make it now.
- **No production code path.** The prototype does not get wired into the running client. If
  TS wins, it is committed as WP-2.2's seed; if Rust wins, the ADR keeps the measurements and
  the prototype is deleted rather than left to rot as a second implementation.

## 5. Result (filled in after measuring)

All four bars passed, so per §3 Rust/WASM did **not** get a full prototype and is rejected on
documented cost rather than on a build.

| Bar | Threshold | Measured |
|---|---|---|
| 1. One core, two drivers, no branches | must hold | 6-case suite green on `node:sqlite` and SQLite-WASM/OPFS from one `suite.ts` |
| 2. OPFS apply throughput | ≥ 5,000/s | 44,651/s (50,000 changes, 1,120 ms) |
| 3. Core size | < 50 KB gz | 708 B gz |
| 4. Headless INV-S testing | must hold | 7 vitest cases, no browser |

Bars 2 and 3 passed by 8.9× and ~70×, confirming §3's own prediction that they would only
fire if something were badly wrong. The decision rests on 1 and 4.

Three things the spike found that no amount of desk research would have:

1. **The replica must live in a dedicated worker** — both OPFS VFSes need
   `createSyncAccessHandle()`, which browsers expose only there. This binds a Rust core
   identically, so it is a constraint on WP-2.2's shape rather than an input to the language
   choice.
2. **The SAH-pool VFS avoids a COOP/COEP tax** the default `opfs` VFS would have imposed on
   the whole product, breaking the sandboxed-iframe extension surface WP-3.6 depends on.
3. **`optimizeDeps.exclude`** is required or Vite serves `index.html` where the `.wasm`
   should be.

The prototype also found a defect in its own first draft: collapsing "already applied" and
"page ascends" into one running cursor made a descending page ([3, 2]) silently drop an
entry — an INV-S3 divergence with no error. Two comparisons now, with the reasoning at the
call site.

## 6. Output

[ADR-017](../adr/ADR-017-sync-client-core.md) — decision, rationale, rejected option with the
measurements that rejected it, and the consequences for WP-2.2/2.3/4.7. The ADR is the
deliverable; the prototype is evidence.

# Phase 2 Review — 2026-08-04 (Claude, requested by Dan)

Scope: all of Phase 2 (WP-2.1, 2.2a/b, 2.3a/b, 2.4, 2.5, 2.6 — PRs #32, #34–#39, all merged)
against roadmap ACs, the commandments, docs/19, the 2026-08-03 premortem's watch list, and the
deferral lists in every `docs/notes/WP-2.*-decisions.md`. Sources: direct code inspection,
`govulncheck`, `pnpm audit`, an ad-hoc `golangci-lint` probe with linters the repo does not enable,
an A/B mutation test of the invariant registry gate, and the full Integrity Gauntlet on
Postgres 18 + SQLite.

**Verdict: the sync engine is real, proven under fault, and better tested than anything before it
in this codebase. The milestone it exists for is not reachable, because the product's screens do
not use it.** Phase 2's work packages are all complete; **M2 is not**, and the gap is one seam that
every WP in the phase correctly declined to build. Two P1s below.

The contrast with Phase 1 is worth stating: that review found a weak *perimeter* around good code
(unscanned dependencies, hardening that existed only in tests). Those are fixed and stayed fixed —
`govulncheck`, `pnpm audit` and the expanded linter set are all in CI and all clean. Phase 2's
weakness is different and narrower: the engine was built to the acceptance criteria, and the
acceptance criteria never said "a user can use it".

## Verified green

- **Every Phase-2 AC passes**, on both dialects. INV-S1–S5 enforced *and* tested; **INV-D1** added
  by WP-2.5. The gauntlet is green on `./...`; `golangci-lint` 0 issues with and without the
  `integrity` build tag; 149 web unit tests; perf smoke inside the docs/09 budgets.
- **Supply chain is clean and the scanning is real.** `govulncheck`: 0 called vulnerabilities.
  `pnpm audit`: no known vulnerabilities. This is the Phase-1 P1 root cause (three scanners
  specified in docs/08, none in `ci.yml`) staying fixed — and the reason this review could spend
  its time on the sync engine instead.
- **The browser pass actually runs in CI** (`e2e:browser`, OPFS in a real worker), so ADR-017's
  platform claim is exercised rather than asserted.
- **Mutation-checking became the house style, unprompted and repeatedly.** WP-2.2b deletes feed
  entries and requires convergence to *fail*; WP-2.4 makes a purge delete `_outbox` and requires
  the conservation test to go red; WP-2.5 neuters `wipeReplica` and requires both AC tests to go
  red. Three WPs in a row independently concluded that a green invariant test is worth only what
  its falsification says it is. That is the single healthiest thing in this phase.
- **The hard design calls were made in decisions files before the code, and they held.** "A command
  is a stored HTTP request replayed through the ordinary gateway" (WP-2.3 §1) turned INV-S2 from a
  property to test into one with no way to express a violation. Nothing in 2.4 or 2.5 had to walk
  it back.
- **Phase-1 P1 item 4 verified fixed**: `cmd/lasterp/main.go` `shutdown` uses `srv.Shutdown(ctx)`
  with its own drain timeout, falling back to `Close()` only on timeout.
- **ADR-021 closed the docs/08 ↔ ADR-017 contradiction** the premortem said would sink WP-2.5, and
  moved the unachievable AC to a numbered WP-4.8 rather than rewording it. The premortem's stated
  fear — "editing a roadmap AC to match an implementation" — did not happen.

## P1 — fix before M2

### 1. The invariant registry gate reads stale agent worktrees, and goes green on a deleted test

`kernel/integrity/catalog_test.go`'s `TestEveryRequiredInvariantHasATaggedTest` walks the repo root
skipping only `node_modules` and `.git`. `.claude/worktrees/` holds full checkouts of this repo at
other commits — gitignored, invisible to CI, present on a developer's machine. The gate harvests
invariant tags from them.

Measured on this tree: **57 test files under `.claude/`, supplying 18 distinct invariant IDs**
(INV-F1–F8, INV-E1–E5, INV-T1–T5, INV-X5).

Proven by A/B, not argued: erasing every `INV-E2` tag from the real tree —

- with the worktree skipped: **FAIL**, naming INV-E2 (correct);
- with it not skipped, i.e. what shipped: **ok**.

So deleting a tagged invariant test — *the specific thing CLAUDE.md forbids and this gate exists to
catch* — passes locally. CI is unaffected (a clean checkout has no worktrees), which is precisely
why it went unnoticed: **the gate was weakest on the machine where it is checked before pushing.**

Fixed in this review's branch, one line, because the review's own evidence depends on the gate
being sound. A stale 176 MB worktree on `wp-1.11` is still registered (`git worktree list`) and
should be pruned separately — it is local hygiene, but it is what made this visible.

### 2. M2 is not demonstrable: the screens do not use the replica

The sync engine is complete and the UI writes online-first. `web/src/sync/` is wired into exactly
two screens — `Conflicts.tsx` (the tray) and `Shell.tsx` (status badge, drain-on-reconnect).
`ObjectForm`, the actual data-entry screen, contains no `useReplica` and no `enqueue`.

This is not a defect; it is a deferral every WP made correctly and none owned. WP-2.3b §13 recorded
it exactly: *"routing `ObjectForm` through the outbox needs the replica to be the read path too,
which is one seam away and is not this WP."* Three WPs later it is still one seam away, and it is
the seam between "Phase 2 complete" and the milestone.

The premortem predicted the shape of this: *"if it runs under two minutes, or every step is a read
plus one draft, the milestone is hollow today, not in December."* Today it would not run at all —
airplane mode gives a blank list, because the list reads the API.

**→ WP-2.7** (proposed below). Also do what the premortem asked and has still not been done: write
the airplane-mode script *first*, so the AC is a user's day and not a developer's seam.

## P2 — tracked, must not vanish

### 3. Three security pins go inert the moment pnpm is upgraded

`package.json` carries `pnpm.overrides` pinning `brace-expansion@1`, `brace-expansion@5` and
`postcss@8` — security pins. pnpm ≥10 **no longer reads that field** and says so as a `[WARN]`,
not an error. Today they hold: CI installs `pnpm@9.15.0` explicitly in all four jobs, and the
lockfile resolves the safe versions (1.1.18, 5.0.9, 8.5.25). But this machine already runs a pnpm
that ignores them, so a local `pnpm install` that regenerates the lockfile drops the pins silently,
and the eventual pnpm bump does the same in CI. Migrate them to `pnpm-workspace.yaml` `overrides:`.

### 4. The feed's throughput ceiling is still unmeasured, and its escape hatch turned out not to exist

The per-tenant advisory lock (`kernel/changefeed/order.go`) is what makes feed id order equal commit
order, and INV-S5 rests on it. Nothing measures it: no test references `serializeAppend` or
`advisoryKey`, and the CI perf smoke spreads load across tenants — the one shape where a per-tenant
lock is free. The premortem asked for a single-tenant N-writer load test before the drain landed;
the drain landed in WP-2.3b and the test does not exist.

This got sharper in WP-2.4, which corrected `order.go`'s stated upgrade path: a per-scope lock is
**not** a relief valve (per-scope ordering under one global cursor re-creates the stranding INV-S5
prevents; doing it properly needs per-scope *cursors* — a rewrite of the reader, `_sync_state`, the
ack path and INV-S5's wording). So the ceiling is unknown *and* the cheap way out was withdrawn.

**Correction to the premortem, verified here:** its "live issue #2" claimed the critical section is
wider than `order.go` says — "spanning every statement after [the first append], not just the
INSERT", with invoice posting appending several times in one transaction. **That does not hold in
this tree.** Both call sites put the append last: `kernel/eventstore/eventstore.go:168` is
`return changefeed.Append(...)`, the final statement of the transaction closure, and
`kernel/metadata/crud.go:201`'s `recordAudit` is followed only by in-memory work on the result map.
`eventstore.Append` takes a `*storage.DB` and opens its own transaction per call, so a posting that
appends several times does so in several transactions, taking and releasing the lock each time.
`order.go`'s "the window is the commit itself" is accurate. The unmeasured ceiling is the real
concern; the widened critical section is not.

### 5. Deferrals that now have no owner

Carried forward from Phase-2 decisions files, none of which have a WP:

- **Row-level / conditional scopes** (WP-2.4 §10) — blocked on `authz` condition evaluation, which
  has been `ErrConditionNotSupported` since WP-0.3 and is referenced by docs/08's CEL design.
- **Per-device scopes** (WP-2.4 §10, WP-2.5 §8) — now expressible since devices are entities.
- **Field-level dirty tracking** for the master-data merge docs/04 §Conflict policy specifies
  ("field-level merge when disjoint fields changed") — WP-2.2b §10 deferred it, WP-2.3 §3 kept it
  deferred. The conflict tray currently treats every same-row edit as a conflict.
- **Multi-tab coordination** (WP-2.2b §5, WP-2.3 §5) — correctness is covered by the exclusive VFS
  handle, so a second tab is *blocked*, not merged. An ERP user having two tabs open is ordinary.
- **The streaming transport** (WP-2.1 §7) — polling converges and push only changes latency, but
  docs/04 §Downstream 3 still promises "~1s".

## P3 — watch list, not defects

- **`GO-2026-5932` in `golang.org/x/crypto` has no fix available** ("Fixed in: N/A") and is not
  called by our code. Nothing to do; re-check when upstream ships.
- **An ad-hoc probe with `noctx`/`contextcheck`/`nilnil`/`errname`** (linters the repo does not
  enable) found 29 hits, all benign on inspection. Two worth recording so they are not re-litigated:
  `contextcheck` flags `shutdown(srv)` for not taking the caller's context — passing it would be
  *wrong*, since that context is already cancelled by the signal and `Shutdown` would return
  immediately. `nilnil` flags `internal/app/sync.go`'s `crudFor` returning `(nil, nil)`; that is the
  documented "unknown, event-sourced, or module disabled" sentinel every caller checks.
- **`kernel/capability`'s `ErrModuleInUse` is a type, not a sentinel**, so `errname` wants
  `ModuleInUseError`. Cosmetic; renaming an exported symbol is not worth a churned API.
- **`_pending` rows can outlive a purge** as orphans keyed to commands that still exist. Harmless
  today (they are cleaned when the command resolves) but it is a set that only shrinks by accident.

## Proposed roadmap changes

- **WP-2.7 Offline-first screens** (Phase 2, **gates M2**). Make the replica the read path for
  metadata-driven screens and route `ObjectForm` writes through the outbox, so the shipped UI is
  the one the sync engine was built for. Prerequisite deliverable: **the airplane-mode script**,
  written first, defining the AC as a complete offline job rather than a seam — the premortem's
  suggestion of *draft → posted invoice, with GL posting as a replayed command* is the right shape,
  and it will immediately surface which of docs/04's "online-required by default" actions M2 needs
  to relax. AC: the script runs end to end with the network off and converges on reconnect, driven
  in a real browser over OPFS, with no step reading the API.
- **The P2 items above want a home.** Suggest folding #3 (pnpm overrides) and #4 (single-tenant feed
  load test) into WP-2.7 as small carried items rather than minting a WP for each; #5's deferrals
  belong in Phase 3+ WPs where their consumers appear, but they should be *named* in the roadmap so
  they stop living only in decisions files.

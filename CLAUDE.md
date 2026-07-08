# CLAUDE.md — Agent Operating Manual for LastERP

You are building LastERP, an open-source, AI-first, offline-capable ERP. Read [README.md](README.md) first, then the doc for the area you're touching. The commandments in README.md are non-negotiable; ADRs in `docs/adr/` are settled — don't relitigate them in code, open an ADR PR instead.

## How to pick up work
1. Work comes as Work Packages (WP) from [docs/11-ROADMAP.md](docs/11-ROADMAP.md). A WP is done only when its acceptance criteria pass in CI.
2. Before coding: read the WP's linked design doc section. If the design is ambiguous, write a short `docs/notes/WP-x.y-decisions.md` stating your interpretation, then proceed — don't stall.
3. Phases are sequential; WPs within a phase can run in parallel. Respect module boundaries: `modules/*` may import `kernel/*`, never each other. Cross-module reactions go through domain events.

## Stack (fixed — see docs/02-TECH-STACK.md)
Go **1.26.4** pinned toolchain — track latest stable patch within 2 weeks of release (server, **no CGO** — modernc.org/sqlite, wazero), PostgreSQL 16+/SQLite behind the storage adapter, NATS JetStream (embedded lib in solo mode), React 19 + TypeScript + Vite + TanStack (web), Extism/wazero (plugins), MCP (AI surface).

## Hard rules
- **Money:** integer minor units + ISO-4217 code. Never float. Rounding/allocation only through `kernel/money` helpers.
- **Financial writes:** append events via the event store; posted documents are immutable; corrections are reversing events. If you find yourself writing `UPDATE` on a posted financial row, stop — you're wrong.
- **Tenancy:** every table gets `tenant_id` (first column of every index) + RLS policy; every request path sets tenant context via kernel middleware. New tables without RLS fail CI.
- **APIs:** generated from metadata first, hand-extended second, hand-built only with justification in the PR. All writes take idempotency keys.
- **Every capability must be reachable via API/MCP.** UI-only features are bugs.
- **SQL:** parameterized only. **Errors:** wrapped with context, problem+json at the edge. **Time:** UTC in storage, always.
- **No new runtime dependencies** (services or heavyweight libs) without an ADR.
- Plugins/AI: never widen a capability or bypass an approval gate to make a test pass.
- **Autonomy (docs/13, ADR-014):** code that lets the system change itself (L2–L4) must route through the customization-package / plugin / PR pipelines — never invent a side door. The constitutional list in ADR-014 is untouchable by any autonomous path.

## Testing bar (per WP; see also docs/09 CI gates and docs/19 — the Integrity Gauntlet)
- **Data integrity is paramount.** Every PR, plugin, and autonomous change passes the Integrity Gauntlet (docs/19) — the invariant catalog (INV-*) is the contract; new invariant-bearing code registers its invariants + tagged tests or CI fails.
- Unit tests for logic; adapter conformance suite must pass on Postgres AND SQLite for storage-touching code.
- Property tests for invariant-bearing code (ledger balance, money allocation, sync convergence). The sync simulation harness (docs/04) is mandatory for any sync-touching change.
- E2E (Playwright) for user-facing flows. Golden files for tax/report calculations.
- Performance budgets (docs/09 table) enforced by CI smoke — don't merge regressions.
- Never weaken/delete a failing invariant test to go green. Fix the code or escalate in the PR.

## Conventions
- Go: standard gofmt/golangci-lint config in repo; table-driven tests; small interfaces at consumer side.
- TS: strict mode, ESLint config in repo; generated types from metadata are the source of truth — never hand-write duplicates.
- Commits: conventional commits (`feat(ledger): …`); one WP = one PR where feasible; PR description links WP and lists AC status.
- Docs move with code: user-facing behavior changes update `docs/`, API changes regenerate OpenAPI in the same PR.
- Naming: the project is **LastERP** (official); binary/CLI `lasterp`; Go module `github.com/iamdoubz/lasterp`; repo https://github.com/iamdoubz/lasterp.

## Definition of done (every WP)
AC pass in CI · tests per bar above · docs updated · no lint/vuln findings · performance smoke green · security-sensitive WPs (authz, sync, plugins, payments) get an explicit threat notes section in the PR.

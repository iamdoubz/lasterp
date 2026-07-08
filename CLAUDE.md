# CLAUDE.md — Agent Operating Manual for LastERP

You are building LastERP, an open-source, AI-first, offline-capable ERP. Read [README.md](README.md) first, then the doc for the area you're touching. The commandments in README.md are non-negotiable; ADRs in `docs/adr/` are settled — don't relitigate them in code, open an ADR PR instead.

<!-- MEMANTO-MANAGED-SECTION -->
## MEMANTO - Your Active Memory Companion

**MEMANTO is not a passive store. It is an active companion agent that works alongside you.**
Don't treat MEMANTO like a static blob you query once and forget. It's a teammate you keep
talking to, every preference, decision, and correction flows through it. MEMANTO remembers,
recalls, and answers so you hold context across sessions, honor prior decisions, and avoid
repeating mistakes the user already corrected.

Every memory operation in this session goes through MEMANTO. There is no exception.

> **CRITICAL**: All `memanto` commands are **shell commands**. Always run them using the Bash tool.
> Never simulate, describe, or "pretend to call" them. If you cannot run the shell, say so explicitly instead of inventing memory state.

### NON-NEGOTIABLE RULES

These are not suggestions. Follow each one on every turn.

1. **Read `MEMORY.md` before doing anything.** It is auto-synced at session start and holds
   the user's preferences, facts, goals, instructions, decisions, and commitments from every
   prior session. You MUST honor what is written there. If you act against it, you are
   breaking continuity the user is paying for.
2. **Search memory before saying you don't know.** If the user asks about past context, an
   earlier decision, a preference, or anything you are unsure about, you MUST run `recall`
   or `answer` first. Saying "I don't have context" without searching is a failure.
3. **Store proactively. Do not wait to be asked.** The moment a memory-worthy event happens
   — a preference stated, a decision made, a fact learned, an instruction given, a goal set,
   a mistake corrected — run `memanto remember` immediately, in the same turn.
4. **Always pass full metadata to `remember`.** Every `memanto remember` call MUST include
   `--type`, `--confidence`, `--provenance`, and `--source <your_agent_name>`. Never let
   these default. Untyped, unsourced memories pollute the agent's recall quality.
5. **One memory operation goes through MEMANTO. All of them do.** Do not keep mental notes,
   in-context scratch pads, or "I'll remember this for next time" promises. If it matters
   beyond this turn, it goes into MEMANTO. If it doesn't, drop it.

### Memory Operations — Use the Right One

MEMANTO gives you three primitives. They are equal-priority. Pick by intent, not by habit.

| You want to... | Use | Why |
|---|---|---|
| Read raw memory chunks and apply them as context | `memanto recall "query"` | Best for context-building, multi-step work, comparing options |
| Get one synthesized, grounded answer to a direct question | `memanto answer "question"` | Best for "what did we decide / prefer / commit to?" — saves you reading and merging |
| Persist something memory-worthy | `memanto remember "content" --type ... --confidence ... --provenance ... --source ...` | Every preference, decision, fact, instruction, goal, lesson |
| See what changed since last time | `memanto recall --changed-since "last 7 days"` | Catching up after a break |
| See the most recent memories | `memanto recall --recent` | Fast context refresh |

Do NOT always default to `recall`. If the user asked a direct question, `answer` is usually
the right tool — it returns a grounded synthesis so you don't burn tokens re-reading raw
chunks.

### When to Call `remember` (Examples — Run Immediately)

- User says *"I prefer tabs over spaces"*:
  `memanto remember "User prefers tabs over spaces for indentation" --type preference --confidence 1.0 --provenance explicit_statement --source <your_agent_name>`
- You decide to use Library X for reason Y:
  `memanto remember "Chose Library X for reason Y; commit abc123" --type decision --confidence 0.95 --provenance inferred --source <your_agent_name>`
- User corrects an approach:
  `memanto remember "User corrected: use pytest, not unittest" --type learning --confidence 1.0 --provenance corrected --source <your_agent_name>`
- A failed approach taught you something:
  `memanto remember "Batch size > 100 fails with TimeoutError" --type error --confidence 0.95 --provenance observed --source <your_agent_name>`

### Command Reference

```bash
# Store — ALWAYS pass full metadata
memanto remember "content" --type <type> --confidence <0.0-1.0> --provenance <provenance> --source <agent_name>

# Recall raw context
memanto recall "query"                              # semantic search
memanto recall "query" --type <type> --limit 10     # filtered search
memanto recall --recent --limit 10                  # newest first, no query
memanto recall --as-of "2026-01-15"                 # state at a point in time
memanto recall --changed-since "last 7 days"        # what changed since

# Synthesized answer (grounded RAG over memories)
memanto answer "question"

# Re-sync MEMORY.md (project-local cache)
memanto memory sync --project-dir .
```

**Memory types** (use the closest fit, do not invent new ones):
`fact`, `preference`, `instruction`, `decision`, `event`, `goal`, `commitment`,
`observation`, `learning`, `relationship`, `context`, `artifact`, `error`.

**Provenance values**: `explicit_statement`, `inferred`, `observed`, `corrected`,
`validated`, `imported`.

**Confidence**: `1.0` for explicit user statements; `0.9-0.95` for strong consensus;
`0.8-0.85` for observed patterns (3+ times); `0.6-0.75` for emerging patterns.

> **Note**: The `memanto-memory` skill contains reference guidelines only (best practices, confidence levels, tagging). It is NOT executable — always use Bash for memanto commands.
<!-- /MEMANTO-MANAGED-SECTION -->

## How to pick up work
1. Work comes as Work Packages (WP) from [docs/11-ROADMAP.md](docs/11-ROADMAP.md). A WP is done only when its acceptance criteria pass in CI.
2. Before coding: read the WP's linked design doc section. If the design is ambiguous, write a short `docs/notes/WP-x.y-decisions.md` stating your interpretation, then proceed — don't stall.
3. Phases are sequential; WPs within a phase can run in parallel. Respect module boundaries: `modules/*` may import `kernel/*`, never each other. Cross-module reactions go through domain events.

## Stack (fixed — see docs/02-TECH-STACK.md)
Go **1.26.4** pinned toolchain — track latest stable patch within 2 weeks of release (server, **no CGO** — modernc.org/sqlite, wazero), PostgreSQL 18+/SQLite behind the storage adapter, NATS JetStream (embedded lib in solo mode), React 19 + TypeScript + Vite + TanStack (web), Extism/wazero (plugins), MCP (AI surface).

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

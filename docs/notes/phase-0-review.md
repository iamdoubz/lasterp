# Phase 0 Review — 2026-07-12 (Claude, requested by Dan)

Scope: all of Phase 0 (WP-0.1–0.9, PRs #1–#9) reviewed against roadmap ACs, the commandments, and docs/19. Sources: git log, MEMANTO-synced MEMORY.md, direct code inspection. Verdict: **Phase 0 is sound — cleared to start Phase 1 after the P1 items below.**

## Verified green
- All 9 WPs merged via PR with conventional commits, per-WP decision notes, DCO + SPDX enforced in CI. AGPLv3 root / Apache-2.0 in sdk/, proto/, kernel/plugins/abi/ — ADR-012 boundary correct. Go 1.26.4 pinned; no-CGO drivers (pgx stdlib mode, modernc.org/sqlite) per ADR-001/002.
- Event store: optimistic concurrency (INV-E2), command_id idempotency (INV-E4), snapshots, read-side upcasters (INV-E3), cursor feed, UPDATE/DELETE-rejecting triggers (INV-E1), 1000-writer torture test on both dialects.
- Tenancy/authz: RLS context, authorize choke point (INV-T2/T4), permission floors (INV-T3); WP-0.6 review caught and fixed a real INV-T1/T2 seam (divergent actor/tenant) — the review loop works.
- Integrity: full INV catalog (E/F/S/T/X) as code, adversarial writer suite, DB grants, **integrity-gauntlet as its own CI job** including the ADR-018 composability suite.
- Metadata engine v1, API gateway (OpenAPI, idempotency, problem+json, rate limiting, capability-disabled responses), i18n (pseudo-locale + RTL tests, hardcoded-string lint), capability solver with 13 builtin manifests (incl. HR) + profiles.
- 41 test files; conformance suite runs identically on Postgres 18 (testcontainers) and SQLite.

## P1 — fix before/at start of Phase 1
1. **CODEOWNERS is missing.** Add `.github/CODEOWNERS`: `kernel/integrity/`, `kernel/authz/`, `kernel/tenancy/`, `kernel/eventstore/`, future payment paths → @iamdoubz. Also confirm GitHub branch protection: required checks = lint, test, **integrity-gauntlet**, build; no admin bypass. (The gauntlet is only as non-skippable as GitHub makes it.)
2. **Gauntlet job scope will silently rot.** It runs `./kernel/integrity/... ./kernel/capability/...` — correct today, but Phase 1 module INV tests will live elsewhere. Convert to a convention now (e.g. build tag `//go:build integrity` + `go test -tags integrity ./...`) so new INV-tagged tests are picked up automatically instead of by remembering to edit ci.yml.
3. **Metadata DDL is create-only** (no ALTER/diff planning) — the "migration planning" half of WP-0.5's AC. Fine for Phase 0; becomes a real gap the first time a Phase-1 overlay adds a field to an existing object. Schedule as WP-1.0a (or fold into WP-1.2's storage work) before tenant overlays evolve in anger.

## P2 — tracked deferrals that must not vanish (add to roadmap)
- **OIDC** (WP-0.3 AC item; deferred pending JOSE library ADR). Password+TOTP is fine for dogfood; OIDC before any external user. Propose **WP-1.9: OIDC AuthProvider + ADR-019 (JOSE lib)**.
- **CEL conditions** stored but not evaluated (unconditional grants only) — lands WP-3.3; note it in WP-3.3's AC explicitly.
- **Event-sourced object support in metadata engine** (CRUD-only today) — WP-1.2 needs it; make it WP-1.2's first task, not a surprise.
- **WP-0.6 accepted nits:** unbounded rate-limit bucket map (memory-growth DoS — cap/LRU it), 401 echoing authenticator error text (info leak), per-process-only limiter (cross-replica limiter arrives with Valkey/ADR-016 at scale), one non-injectable time.Now(). First two are cheap; do them in any Phase-1 gateway-touching WP.
- **Full DB role separation** (docs/19 layer 3) deferred "until posting exists" per kernel/integrity/grants.go — correct call; belongs in WP-1.2's AC.

## P3 — process notes
- **Docs divergence:** repo docs are now canonical (Postgres pinned 18+ per Dan, 2026-07-07); the planning folder `C:\Users\dadous\Claude\Projects\ERP` still says 16+ and lacks repo-era changes. Recommend adding a "superseded — see repo docs/" banner there rather than dual-maintaining.
- **Shared-working-dir branch clobbering** (seen 2026-07-08 in reflog): enforce the worktrees-only rule for concurrent sessions; never two agents on the repo's main checkout.
- **Parallel-branch CI drift** (wp-0.6 missing i18n-lint.sh): when main gains a CI-required file, open branches must merge main before their PRs go green — worth a line in agent-setup README.
- MEMANTO hygiene is good (37 memories, corrections captured, PII lesson recorded). Keep the `memanto agent activate lasterp` rule — the wrong-agent sync trap is documented and real.

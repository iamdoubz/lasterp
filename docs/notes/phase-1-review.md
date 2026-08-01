# Phase 1 Review — 2026-07-31 (Claude, requested by Dan)

Scope: all of Phase 1 (WP-1.0a–1.9, PRs #11–#24 — all merged) against roadmap ACs,
the commandments, docs/19, and the deferral lists in every `docs/notes/WP-*-decisions.md`.
Sources: direct code inspection, `govulncheck`, `pnpm audit`, an expanded `golangci-lint` probe,
and the full Integrity Gauntlet on Postgres 18 + SQLite. Verdict: **Phase 1 is functionally
complete and the invariant machinery is real. What is weak is the perimeter around it — supply-chain
scanning that was specified and never built, deployment hardening that exists only in tests, and
deferrals recorded as "later WP" where no such WP exists.** Cleared for M1 after the P1 items.

## Verified green

- Ledger, tax, invoicing/AR, receipts, reporting, dashboards, i18n and the web client all land
  their ACs. Full gauntlet green on both dialects; `golangci-lint` 0 issues; 64 web unit tests;
  12 Playwright specs with axe-core scans; perf smoke well inside the docs/09 budgets (read p95
  6.2ms/100ms, write 76.1ms/300ms, dashboard 19.4ms/100ms).
- Invariants enforced *and* tested: INV-E1–E5, INV-T1–T4, INV-F1–F6, INV-F8. The WP-0.10
  `//go:build integrity` auto-discovery convention held — every Phase-1 module joined the gauntlet
  with zero `ci.yml` edits.
- The money type makes a float amount unrepresentable outside constructors; allocation conserves
  every minor unit under 400k randomized iterations. Posted documents are immutable by trigger on
  both dialects. Settlement is derived, never written onto the invoice.
- WP-1.8 caught a genuine reporting lie before it shipped (all-time metrics rendered as
  period-over-period comparisons); WP-1.9's own tests caught two real defects in the code they
  were written against. The review loop is doing its job.

## P1 — fix before M1 (→ WP-1.10)

1. **A reachable CVE is shipping.** `golang.org/x/text v0.37.0`, GO-2026-5970 (infinite loop on
   invalid input), reachable via `storage.Open` → `sql.Open` → `norm.Form`. Fixed in v0.39.0;
   current v0.40.0. Alongside it: `golang.org/x/crypto v0.51.0` carries 14 advisories (13 fixed in
   v0.52.0, current v0.54.0) — not currently reachable, but it is a direct dependency and it is the
   crypto library. `modernc.org/sqlite v1.53.0 → v1.55.0`.
2. **`govulncheck` and `npm audit` are not in CI, and no SBOM is published** — all three are
   specified in docs/08 ("Dependencies: pinned + `govulncheck`/`npm audit` in CI; SBOM published per
   release"). This is the actual defect; item 1 is its symptom. `pnpm audit` currently reports 9
   advisories (1 critical, 5 high) — all dev tooling (vitest/vite/esbuild/postcss/brace-expansion),
   none in the shipped bundle, but the critical one is an arbitrary-file-read on developer and CI
   machines.
3. **No security headers anywhere in the product.** No CSP, `X-Content-Type-Options: nosniff`,
   HSTS, `X-Frame-Options` or `Referrer-Policy` are set on any response. docs/08 lists CSP and HSTS
   as shipped hardening. This matters more than the generic case here: the SPA renders
   tenant-authored strings through the metadata renderer, and PDF/CSV/XLSX downloads are served from
   the same origin as the session cookie.
4. **`http.Server` has no timeouts** (`cmd/lasterp/main.go`, both `serve` and `dev`) — gosec G112,
   Slowloris. Shutdown also uses `srv.Close()` rather than `srv.Shutdown(ctx)`, so every deploy
   severs in-flight requests.
5. **Go is pinned at 1.26.4 while 1.26.5 is out.** ADR-001's own policy is "adopt new patch releases
   within 2 weeks (they carry security fixes)", so this is overdue by the repo's rule, not by taste.
   Seven places: `go.mod`, five `ci.yml` steps, `docs/02`, ADR-001. Separately, **`go.mod` has no
   `toolchain` directive** even though ADR-001 specifies pinning via it and WP-0.1-decisions §2
   records it as set — it is not there.
6. **DB role separation exists only in tests.** `integrity.EnforceAppendOnlyGrants` and
   `EnforceLedgerPipelineGrants` are called from six `testdb_test.go` files and nowhere else — not
   the CLI, not the Dockerfile, not the Helm chart, not any doc. A real deployment connects as the
   migrating owner with no REVOKEs applied. Mitigated: append-only triggers on `events` (0012) and
   `audit_log` (0021) fire regardless, and `FORCE ROW LEVEL SECURITY` keeps RLS live against the
   owner — so this is lost defense in depth, not an open door. But **INV-F5's storage-layer claim is
   true in CI and false in production**: without `REVOKE INSERT ON events`, nothing stops a future
   code path writing the log directly. WP-1.4b §7 deferred this to "WP-10.x deployment"; WP-10.1 is
   "topology bundles + `lasterp doctor`" and does not mention it. Unhomed until now.
7. **`PasswordTOTPProvider` is dead code in production.** Nothing outside its own unit tests
   constructs it; `internal/app/sessions.go` reimplements password+TOTP inline — and the inline copy
   carries a security property the provider lacks (dummy-bcrypt timing equalization for unknown
   users). So `AuthProvider`, an interface whose entire purpose is interchangeable implementations,
   has exactly one production caller: the OIDC one added in WP-1.9. Two copies of an authentication
   decision is a divergence risk on the worst possible path. Converge or delete — not both as they
   stand.
8. **`.golangci.yml` enables nothing beyond the v2 defaults.** For a codebase with money and
   hand-written SQL, `gosec`, `rowserrcheck`, `sqlclosecheck`, `errorlint`, `bodyclose` and `nilerr`
   are free. An ad-hoc run with those found 17 issues: 2 real (item 4), 3 worth a look (below), and
   the rest false positives — gosec cannot see the length guard at `invoicing/post.go:42`, and flags
   `crypto/sha1` in TOTP which RFC 6238 mandates. Enable the set with the false positives annotated.

Smaller items to fold into the same WP:

- `modules/reporting/card.go:97` swallows **every** error from the prior-period evaluation, not just
  the permission and missing-period cases its comment reasons about. A database outage would render
  as "no comparison available" indefinitely. Narrow it to the expected sentinels.
- `internal/app/response.go:68` writes `r.URL.Path` into `log.Printf` unsanitized (gosec G706).
  Log-forging is a minor issue generally and a less minor one in a product whose pitch includes
  hash-chained audit trails.
- `MEMORY.md` is untracked and not in `.gitignore`. It is the MEMANTO project cache; it appears in
  every `git status` and will eventually be committed by accident.
- Tax/FX reference-data **read** API (WP-1.4b deferred the read half; only writes exist, reads go
  through the domain funcs). Small, and its absence is felt the moment anyone wants to see what rate
  applied.

## P2 — tracked deferrals that must not vanish (added to the roadmap)

Deferrals with a real home need no action: sync/offline → Phase 2 · formula DSL, dimensions, ~150
metrics → WP-4.13 · due-date aging, payment terms, dunning, payment matching → Phase 4 banking ·
cash-flow statement, GL detail, tax returns, PDF/Parquet export → later reporting WPs · INV-F7 →
WP-4.4 · INV-S*/INV-X* → Phases 2/3/6.

These had none, and now do:

- **Enum option validation and field-type validation in the metadata engine.** WP-1.6 §5 called this
  "the one finding that outlives this WP" and pointed at "the ADR-006 WP" — which does not exist.
  It is also broader than recorded: `CRUD.validate` (`kernel/metadata/crud.go:58`) checks
  **required-ness and nothing else** — no type check, no enum check. `FieldEnum` falls to the
  `default:` branch and becomes a free-text column. Six enum fields are in production schemas
  (`Contact.kind`, `Account.type`, `Period.status`, `Invoice.status`, `Receipt.status`), of which
  `Account.type` and `Contact.kind` are reachable through generic CRUD — so
  `POST /api/v1/account {"type":"banana"}` is accepted into the books today. Reports mitigate
  honestly (explicit `unclassified` bucket, nothing silently dropped), which is why it has not bitten.
  → **WP-1.11**, which also absorbs the UI-descriptor deferral from WP-1.5 §2.
- **Secrets vault.** Specified in docs/08, referenced by docs/20, and the stated blocker for
  per-tenant OIDC configuration (WP-1.9 §2). No WP anywhere. Earliest hard dependency is WP-3.1's
  capability-gated `secrets.get`. → **WP-3.0**, at the head of Phase 3.
- **TOTP enrollment.** `ValidateTOTP`/`EnableTOTP` exist and the login route accepts a code, but
  nothing lets a user turn it on — so M1's own text ("password+TOTP acceptable for dogfood") is not
  currently true; the available level is password-only. → **WP-1.12**.
- **Metadata-declared action routes.** ADR-009 says "metadata-declared first, hand-extended second,
  hand-built only with justification in the PR". All ~30 action routes are hand-registered structs;
  WP-1.4b deferred the metadata `actions:` block to "a later WP" that does not exist. Not urgent —
  the routes work and are documented in OpenAPI — but it is a standing ADR-009 exception that should
  be either scheduled or written into ADR-009 as accepted. → folded into **WP-3.7** (developer
  platform), whose live-spec work touches the same surface.

## P3 — watch list, not defects

- **`ledger.BalancesForPeriods` reads the tenant's entire event log per call**, and it sits on the
  dashboard render path — every KPI card with a period comparison. The measured 19.4ms p95 is against
  the demo book (dozens of events). The upgrade path is written down at the call site (period-grained
  projection on the same cursor); it needs a trigger condition, not a rewrite. Same family:
  `EnsureBalances` first-call catch-up, `settlement.appliedToInvoiceTx` scanning all posted receipts,
  rate-limiter O(n log n) eviction. All carry `ponytail:` comments naming their ceiling.
- **14 capability manifests, 5 built modules.** Intentional forward-declaration per ADR-018 and
  harmless — nav is built from registered schemas, so there are no phantom screens — but
  `capability.Enable("payroll")` currently succeeds and does nothing. A "declared but not installed"
  state would be more honest whenever the module list next changes.
- **docs/09 claims CI runs a k6 load smoke and a nightly full-scale load test.** Neither exists. The
  Go perf test that does exist is the right thing for now; the doc is what is wrong. Fix the doc or
  schedule the tests with WP-5.1.
- **`RefreshSession`'s doc comment is wrong** (claims the old access token keeps working; the
  implementation replaces `token_hash`, so it stops immediately). Behaviour is the safer of the two.
  Comment-only fix, noted in the WP-1.9 PR.
- The Docker-heavy gauntlet run overwhelms Docker Desktop on Windows when packages run in parallel,
  producing spurious `rootless Docker is not supported on Windows` failures across many packages.
  Re-running the affected packages with `-p 1` passes. Worth a line in the agent-setup README so it
  is not mistaken for a real failure.

# WP-0.10 decisions — Phase-0 review follow-ups

Source: [phase-0-review.md](phase-0-review.md) P1/P2 items, roadmap WP-0.10 entry.
This WP is wiring/hardening, not new domain logic. No new INV-* is registered.

## Invariants touched
- **All INV-E\*/INV-T\*** — indirectly, via part (b). The *discovery* of their
  tagged tests (the registry gate + adversarial suite) moves from a
  hard-coded package list to a build-tag convention. Enforcement is unchanged;
  the meta-guard `TestEveryRequiredInvariantHasATaggedTest` is disk-scan based
  and tag-agnostic, so it keeps covering INV tests in *untagged* packages
  (eventstore, authz, tenancy, metadata) too.
- **INV-T1 / INV-T2** — the gateway front door (part c). The tenant-mismatch
  fail-closed guard is left exactly as WP-0.6 built it; part (c) only touches
  the rate-limit bucket map, the 401 body text, and the idempotency clock.
  None of these alter authz/tenant enforcement.

## Decisions

1. **(a) CODEOWNERS** — the six paths named in the roadmap verbatim
   (`kernel/integrity/ kernel/authz/ kernel/tenancy/ kernel/eventstore/
   .github/ scripts/` → `@iamdoubz`). CODEOWNERS only *requests* review; it is
   only enforcing if GitHub branch protection requires code-owner review. That
   branch-protection toggle is a repo-settings action the agent can't perform —
   flagged in the PR for Dan, as phase-0-review P1 #1 already notes.

2. **(b) Gauntlet auto-discovery via `//go:build integrity`.**
   - Tag every integrity/composability test file with `//go:build integrity`:
     `kernel/integrity/{adversarial,catalog,testdb}_test.go` and
     `kernel/capability/{gateway,solver,state,testdb}_test.go`. Whole-package
     tagging (incl. the shared `testdb_test.go` harness) keeps each package
     internally consistent under both build modes — no half-tagged package
     where a normal build sees a helper but not its only caller.
   - CI `integrity-gauntlet` job: `go test -count=1 -tags integrity ./...`
     (was the hard-coded `./kernel/integrity/... ./kernel/capability/...`).
   - **Consequence, accepted:** `-tags integrity ./...` also re-runs every
     *untagged* test. The two jobs run on parallel runners, so wall-clock CI is
     unchanged; the cost is extra runner-minutes, bought in exchange for
     zero-ci.yml-edit discovery of Phase-1 module INV tests (the WP's goal).
     Build tags are additive — there is no way to run *only* tagged tests
     across `./...`, so this redundancy is inherent to the chosen mechanism.
   - Integrity/composability tests no longer run in the plain `test` job
     (`go test ./...` excludes the tag). They run in the gauntlet instead —
     the correct home for them.
   - **Discovery canary:** one `//go:build integrity` test in `kernel/idgen`
     (a leaf package with no other integrity test) proves the tag is picked up
     *outside* `kernel/integrity` — the AC's "dummy tagged test" requirement,
     kept permanently as a wiring smoke test. Placed inside an existing package
     (not a fresh dir) so a normal build still has buildable files there and
     `go test ./...` doesn't error with "build constraints exclude all Go
     files".
   - **Lint coverage:** add `build-tags: [integrity]` to `.golangci.yml` so the
     tagged files stay linted (otherwise golangci-lint, building without the
     tag, would silently skip them and let them rot).

3. **(c) WP-0.6 gateway nits.**
   - **Bucket-map cap:** `rateLimiter` gets a hard cap (`maxBuckets`). When full
     and a new key arrives, evict the oldest 1/8 by `last` in one O(n) pass
     (amortized O(1)/insert; avoids O(n²) under a unique-key flood — the DoS
     case). Safe because an evicted idle bucket would have refilled to full
     anyway, so eviction never changes a legit caller's limit outcome.
     `// ponytail:` names the ceiling; a heap/LRU-list is the upgrade path if
     churn ever exceeds the cap.
   - **401 body:** stop putting `err.Error()` from the Authenticator into the
     problem `Detail` (info leak — line 194). Body becomes a generic
     "authentication required"; the detail is logged-only concern, not built
     here. Test asserts the 401 body does not contain the authenticator's error
     text.
   - **Injectable clock:** the rate-limiter clock is already injectable
     (`Config.Now` → limiter). The one remaining direct `time.Now()` in the
     gateway is `idempotency.go` (the reservation `created_at`); thread the same
     `now` into `idempotencyStore` so the whole gateway shares one clock.

## Non-goals (deferred, not this WP)
- GitHub branch-protection settings (repo admin action — PR note only).
- Per-replica/distributed rate limiter (arrives with Valkey/ADR-016 at scale,
  per phase-0-review P2).

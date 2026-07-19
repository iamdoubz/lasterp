# Phase 1 midpoint review (after WP-1.0a–1.4)

Date: 2026-07-18. Reviewed everything merged in Phase 1 so far (PRs #11–16:
WP-1.0a schema evolution, WP-1.1 money/FX, WP-1.2 ledger in two PRs, WP-1.3 tax,
WP-1.4 contacts + invoicing) against the roadmap, the commandments, and the
invariant catalog. Companion to [phase-0-review.md](phase-0-review.md).

## What holds

- All five WPs merged green: full gauntlet on both dialects, INV-F1–F6, E1–E5,
  T1–T4 all `TestRequired: true` with tagged tests.
- Layering held: modules import kernel only, except invoicing → ledger/tax via
  the declared-`requires:` precedent (WP-1.4-decisions.md §1, now codified in
  CLAUDE.md).
- Deliberate deferrals (FX gain/loss, credit notes/void, multi-currency
  invoices, cash rounding, contract-phase DDL, cross-rate pivoting) are all
  flagged in the per-WP decisions docs; none block M1.

## Finding 1 — nothing is reachable outside tests (→ WP-1.4b)

`cmd/lasterp/main.go` still serves `api.NewMux()` — the zero-config WP-0.1
bootstrap handler (health + hello + empty OpenAPI). No non-test code opens a DB,
runs migrations, calls any module `Register()`, or builds the gateway with
objects/authenticator. Every Phase-1 capability therefore violates the
"every capability must be reachable via API/MCP" commandment: the accounting MVP
exists only inside the test harness.

Compounding it, the gateway is metadata-**CRUD**-only: no routes exist for
event-sourced objects (JournalEntry) or lifecycle verbs — post invoice, reverse
entry, close/reopen period, tax/FX rate admin, capability enable/disable.
WP-1.5's Playwright AC ("invoice lifecycle e2e") is unimplementable until both
exist; left implicit, WP-1.5 silently doubles in size.

**Resolution:** roadmap gained **WP-1.4b Boot assembly + action surface**,
blocking WP-1.5. The tax/FX reference-data write authz seam (deferred from
WP-1.1/1.3 on "no API surface yet" grounds) must land there — the deferral is
only safe while the surface doesn't exist.

## Finding 2 — AR receipts owned by nobody (→ folded into WP-1.6)

WP-1.4 excluded payments per M2 scope, but WP-1.6's AR aging is specified
against payment-reduced balances, and M1 ("a small firm can invoice and keep
books") implies recording a customer payment. Phase-4 banking covers statement
matching, not basic receipt entry; a manual GL journal balances the books but
never marks the invoice paid.

**Resolution:** WP-1.6 now includes minimal AR receipt recording (payment → GL
via declared template + invoice paid/partial status). Full matching/dunning
stays in Phase 4.

## Doc fixes applied with this review

- CLAUDE.md module-boundary rule updated to codify the declared-`requires:`
  precedent (was an absolute "never each other", contradicting merged WP-1.4).
- Roadmap status header updated (was frozen at 2026-07-12 / "next up WP-0.10").
- WP-1.4b and the WP-1.6 receipt amendment added to the roadmap.

## Watch list (no action now)

- INV-F5's "declared template" is a Go function (`buildInvoiceJournal`), not
  data — fine for v1; revisit when templates become metadata (plugins/Phase 3).
- SQLite has no DB roles, so INV-F5/role-separation is Postgres-only; the
  trigger belt still covers SQLite. Accepted platform asymmetry.
- Roadmap lists WP-1.8 before WP-1.7 — intentional ordering, not a typo.

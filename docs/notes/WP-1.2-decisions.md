# WP-1.2 decisions & scope — Ledger module (M1)

**Status: unblocked, planning (2026-07-17).** "The heart; built first, hardest
gates" (docs/10). Depends on WP-1.1 money (merged), WP-1.0a metadata evolution
(merged), WP-0.4 eventstore, WP-0.8 integrity foundation (all merged).

## Dependency check
WP-1.2 is a **module** WP (`modules/ledger`), so the gate "never start a module WP
before WP-0.8 is green" applies — and WP-0.8 is merged. Modules import `kernel/*`
only, never other modules (CLAUDE.md); cross-module reactions go through events.

## Invariants this WP touches (docs/19) — lands INV-F1/F2/F3/F5
- **INV-F1** Every journal entry balances (Σdebits = Σcredits per currency, to the
  minor unit). Enforced in the posting pipeline **and at the storage layer**
  (Postgres: the SECURITY DEFINER posting function re-checks; SQLite: the pipeline
  is the storage owner in the single trusted process).
- **INV-F2** Posted entries are immutable; corrections are reversing entries only.
  Enforced by construction: each entry is a one-event stream on the append-only
  `events` table (INV-E1 trigger + grants); a reversal is a *new* entry with negated
  lines referencing the original.
- **INV-F3** No posting into a closed period; close is monotonic (reopen is a
  privileged, audited action). Enforced in the pipeline + posting function.
- **INV-F5** Financially-relevant documents post to GL only through their declared
  path — for the ledger itself this is "no direct ledger writes outside the posting
  pipeline," which the DB role separation below makes literally true on Postgres.
- **INV-E5** Projections are a pure function of the log — the trial-balance
  projection equals an independent event fold (fuzzed).
- **INV-T2/T4** every post is authorized + attributable (actor, command, timestamp).
- **INV-E1/E2/E4** inherited from the event store (append-only, optimistic
  concurrency, command_id idempotency).

## PR split (Dan, 2026-07-17: "land foundation first")
WP-1.2 ships as two PRs; the WP's AC is fully met only after **PR-B**.
- **PR-A (branch `wp-1.2`, this PR):** event-sourced metadata support
  (`metadata.EventSourced`), Account (CRUD), Period (CRUD + monotonic close),
  JournalEntry (event-sourced), the `Post`/`Reverse` pipeline with Go-side balance +
  open-period validation, and immutability via the append-only `events` table.
  **Flips INV-F1, INV-F2, INV-F3.** In PR-A these are enforced at the **posting
  pipeline choke point** (docs/19 layer 3) plus the append-only trigger (INV-F2/E1,
  storage); on SQLite the pipeline is the storage owner (single trusted process).
- **PR-B (branch `wp-1.2b`, DONE):** the Postgres `SECURITY DEFINER` `append_event` +
  `ledger_post_entry` functions that re-enforce balance/open-period **in the
  database**, revoking direct `INSERT` on `events` from the app role and routing
  `eventstore.Append` through `append_event` — **full DB role separation (INV-F5)** —
  plus the materialized `ledger_balances` projection and the projection==fold fuzz
  (**INV-E5**). PR-A's decision-4 Postgres pieces below are PR-B's work; PR-A notes
  the small check-then-append TOCTOU window that PR-B's atomic function closes.

## Ambiguities resolved

**1. Objects and persistence.**
- **Account** — CRUD (existing metadata engine). Chart of accounts as a tree:
  `code` (required, indexed), `name` (required), `type` (enum
  asset/liability/equity/income/expense — sets normal balance), `parent` (link
  Account, optional), `currency` (optional; empty = multi-currency-capable).
- **Period** — CRUD with a monotonic close. `code` (e.g. "2026-01"), `start_date`,
  `end_date`, `status` (open/closed). `Close`/`Reopen` are authorized, audited
  operations; reopen is privileged (INV-F3). Full period-close-as-events is not
  needed for the AC (the audit_log records the transition).
- **JournalEntry** — **event-sourced**. One stream per entry
  (`stream_id = entry id`), exactly one `ledger.entry.posted` event at version 1 —
  so an entry is immutable by construction (INV-F2). A reversal is a new entry-stream
  whose posted event carries negated lines and `reverses_entry_id`.

**2. "Extend the metadata engine with event-sourced object support" (the WP's first
task) = event-sourced objects become first-class metadata objects, not a generic
YAML aggregate DSL.** `metadata.NewCRUD` rejects `persistence: event_sourced` today
(WP-0.5 decision 2). **Decision:** the metadata engine gains (a) validation +
registration for event-sourced objects (permissions, projection field schema,
surfaced through the object/capability registry like any object) and (b) a thin
shared write choke point — `metadata.EventSourced` — that applies the same
`authz.Authorize` + `tenancy.WithTenant` + `audit_log` discipline to an event-sourced
write that `CRUD` applies to a row (INV-T2/T4 uniformly). The **command → event →
fold** domain logic stays as Go in the owning module. A declarative YAML command/fold
DSL has exactly one consumer today (ponytail: no framework for one caller); invoicing
(WP-1.4) and inventory (WP-4.4) are the second/third consumers of the *choke point*,
which justifies that thin shared piece but not a full DSL.

**3. Single-currency journal entries in v1.** INV-F1 is "per currency." **Decision:**
a v1 entry is in exactly one currency; every line is a debit XOR credit in that
currency; balance is Σdebit = Σcredit in minor units. Multi-currency journal entries
(and realized/unrealized FX gain-loss routines, docs/10 M1) are deferred — flagged,
not silently dropped. Amounts are `kernel/money` minor units.

**4. Storage-enforced invariants + full DB role separation (the crux; docs/19 layer
2+3, deferred from WP-0.8 per `kernel/integrity/grants.go`).**
- **Postgres:** two `SECURITY DEFINER` functions owned by the migration/owner role:
  - `append_event(...)` — the guarded INSERT into `events` (version/command_id
    uniqueness stay index-enforced exactly as today; the function just relocates the
    INSERT to a pipeline-owned path).
  - `ledger_post_entry(...)` — re-validates balance (Σdebit = Σcredit) and open-period
    **in SQL**, then appends the posted event; rejects unbalanced / closed-period with
    a SQLSTATE the Go layer maps to `ErrUnbalanced` / `ErrClosedPeriod`.
  Then **revoke INSERT (and the already-revoked UPDATE/DELETE/TRUNCATE) on `events`
  from the app role**, so the app role can write the log *only* through these
  definer functions — "app role provably cannot write protected tables outside
  pipeline-owned paths." `kernel/eventstore.Append`'s Postgres INSERT is routed
  through `append_event`; its Go-level command_id/version handling is unchanged
  (the same unique indexes still fire). SQLite path unchanged.
- **SQLite:** no roles / no SECURITY DEFINER; solo mode is a single trusted process
  (ADR-005). Balance + open-period are enforced by the Go posting pipeline (the
  storage owner in solo mode); the append-only trigger blocks UPDATE/DELETE. This
  per-dialect split is the same precedent `grants.go` already sets for the WP-0.8
  revokes. The invariant tests assert rejection on **both** dialects.

**5. Trial balance = a rebuildable projection compared against an independent fold.**
A `ledger_balances` projection (per account net, per tenant) is maintained from the
`ledger.entry.posted` feed and is rebuildable from `events` (INV-E5). `FoldTrialBalance`
computes the same result independently from the event stream. The AC property test
posts randomized valid entries and asserts **projection == fold** (this is also the
INV-E5 discharge for the ledger). Normal-balance sign is applied per account type so
the trial balance's debit and credit columns each total equally (a second INV-F1-level
check across the whole ledger).

**6. Reversal.** `ledger.Reverse(entryID)` posts a new entry whose lines are the
original's debits/credits swapped, carrying `reverses_entry_id`; the original is never
touched (INV-F2). Reversing into a closed period is refused (INV-F3) unless a future
"reverse into current open period" option is added (deferred).

**7. Wiring.** The `ledger` capability manifest gains its `objects: [Account, Period,
JournalEntry]`. The posting function + grant changes ship as migrations (Postgres
`.postgres.sql`; SQLite no-ops) plus a runtime `EnforceLedgerPipelineGrants(ownerDB,
appRole)` extending the existing grant helper.

**8. Out of scope (flagged):** multi-currency entries + FX gain/loss; recurring
entries; dimensions beyond a simple per-line dimension tag (cost center/project as
optional line fields only — no dimension master objects yet); GL posting *templates*
for non-GL documents (that's INV-F5 for invoicing/AP, WP-1.4+); period-close as an
event-sourced aggregate; report formatting (P&L/balance-sheet are WP-1.6 — WP-1.2
ships the trial-balance projection they build on).

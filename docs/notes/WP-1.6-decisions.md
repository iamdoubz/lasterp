# WP-1.6 — Reports v1 + metrics layer + AR receipts: interpretation & decisions

Roadmap entry ([docs/11-ROADMAP.md](../11-ROADMAP.md)): *"Reports v1 + metrics layer (amended per
21-REPORTING-DASHBOARDS.md): trial balance, P&L, balance sheet, AR aging from projections; CSV/XLSX
export; metrics layer v1 + drill-down. **Includes minimal AR receipt recording** (payment → GL via
declared template + invoice paid/partial status)… AC: report/metric totals reconcile with event-fold
oracle; permission-leak suite green; recorded receipt reduces AR aging and flips invoice status."*

Design inputs read: [docs/21](../21-REPORTING-DASHBOARDS.md) (§1 metrics layer, §2 reports, build
plan WP-1.6a), [docs/19](../19-DATA-INTEGRITY.md) (always), [ADR-006](../adr/ADR-006-metadata-customization.md),
[ADR-003](../adr/ADR-003-event-sourcing.md), [docs/03](../03-DATA-MODEL.md), [docs/09](../09-SCALABILITY-DEPLOYMENT.md),
plus the existing `modules/ledger` projection and `modules/invoicing` posting pipeline.

---

## 1. The headline problem: "flips invoice status" collides with INV-F2

The AC says a recorded receipt *"flips invoice status"*. A posted invoice is **immutable** — INV-F2,
enforced by a storage trigger that rejects any UPDATE where `OLD.status = 'posted'`
(`invoicing/schema.go:enforceInvoiceImmutability`). Writing `status = 'paid'` onto a posted invoice
would require weakening that trigger. CLAUDE.md is explicit: *"If you find yourself writing UPDATE on
a posted financial row, stop — you're wrong."*

**Decision: settlement state is derived, never stored on the document.** The invoice row is untouched
forever after posting. `open / partial / paid` is computed from the receipts applied against it —
exactly commandment 1 ("financial state is derived from immutable events"). `GetInvoice` gains a
derived `SettlementStatus` + `SettledMinor` / `OutstandingMinor`; `status` keeps its existing document
meaning (`draft` / `posted`).

This satisfies the AC's *intent* — record a receipt, the invoice reads as paid/partial and drops out
of aging — without the AC's literal reading, which would require breaking the invariant the WP is
supposed to be reconciling against. Flagged rather than quietly reinterpreted.

**Rejected:** relaxing the trigger to permit a `status`-only transition. It turns "posted documents
are immutable" into "immutable except the column we happened to need", and the next WP gets the same
exemption for a different column.

## 2. Receipts are a posting document, with the same rigour as invoices

`Receipt` is a CRUD object with a draft→posted lifecycle mirroring `Invoice`, **not** a bare GL entry:
it carries who paid, when, how much, against which invoices.

- **Declared GL template (INV-F5)** — `buildReceiptJournal`, the only path to the ledger:
  `DR bank_account (Σ applied)` / `CR ar_account (per invoice's own AR account, grouped)`. Balances by
  construction (INV-F1).
- **Gapless numbering (INV-F6)** via the existing `document_number_series` (`receipt` series), assigned
  at acceptance, after the GL post — same ordering as invoices so a failed post consumes no number.
- **Immutable once posted (INV-F2)** — the same trigger pattern on `obj_receipt`.
- **Applications** (`[{invoice_id, amount_minor}]`) are stored on the receipt, so "which invoice did
  this settle" is a recorded fact, not a guess. That is what makes settlement derivable and drill-down
  possible.

## 3. NEW invariant: INV-F8 — settlement never exceeds the document it settles

Receipts introduce a financial rule nothing in the catalog covers: **Σ applied against an invoice may
never exceed that invoice's gross**, and no application may be negative. Over-application would create
a negative receivable that silently corrupts aging and DSO, and it is exactly the class of thing a
property test catches and review does not.

**Decision: register `INV-F8` in `kernel/integrity/catalog.go`** (`LayerPipeline`, `TestRequired: true`)
with a property test tagged accordingly, per CLAUDE.md ("new invariant-bearing code registers its
invariants + tagged tests or CI fails"). Enforced inside the receipt-posting transaction, re-reading
prior applications under the row lock so two concurrent receipts cannot both pass the check.

## 4. Reports are pure folds over the log, with the projection as a cache

The AC — *"report/metric totals reconcile with event-fold oracle"* — is **INV-E5** restated
("projections are pure functions of the log: rebuild(events) ≡ projection"). `ledger.FoldTrialBalance`
already exists and its doc comment already says it is "the fold WP-1.6 reports build on".

**Decision:** every financial report is a pure function `(TrialBalance, []Account) → Report`. Reports
read the materialized `ledger_balances` projection at runtime, and the test suite runs each report
twice — once over the projection, once over a fold of the raw event log — and asserts equality. No
report reaches into `obj_*` tables for money.

- **Trial balance:** account → net, plus debit/credit presentation columns; asserts Σ = 0 per currency.
- **P&L:** `income` + `expense` accounts over a period; net income = Σincome − Σexpense (sign-normalized
  by normal balance).
- **Balance sheet:** `asset` / `liability` / `equity`, plus current-period net income as retained
  earnings, asserting `assets = liabilities + equity` — the statement's own reconciliation.
- **AR aging:** per contact, outstanding = invoice gross − applied receipts, bucketed by age.

### 4a. The projection nothing refreshed — resolved in PR-B

PR-A found that `ledger_balances` was written **only** by an explicit
`RebuildBalances` call, and no product code path made one. Posting did not update it. So the
projection was empty in a running system, and reports were about to depend on it.

**Decision: catch up at read time, via a cursor.** `ledger_balance_cursor` records the last event id
folded in; `EnsureBalances` folds only what is new and advances the cursor **in the same transaction**,
so what was folded and what the cursor claims cannot diverge. `Load` calls it before every report.

Rejected: **maintaining the projection inside `Post`.** On Postgres the write path goes through a
SECURITY DEFINER pipeline function that owns the append (INV-F5); hanging a second write off it would
either widen the app role's grants or leave a window where a crash between the two makes the
projection silently wrong. Read-time catch-up is exact by construction — there is no staleness window
to reason about.

This also makes the INV-E5 test **non-vacuous**, which matters more than it sounds. Previously the
projection was written by the same full fold the oracle uses, so comparing them compared the fold to
itself. Now incremental catch-up and full re-fold are genuinely different code paths, and
`TestIncrementalCatchUpMatchesFullRebuild` is a real assertion.

*ponytail:* catch-up is O(new events) per read, and the whole log on first call. If a tenant's feed
outgrows that, move catch-up to a background worker driven by the same cursor — the read path does not
change.

## 5. Account-type classification: an existing hole this WP would otherwise inherit

`ledger.CreateAccount` validates `type` against the closed set
`{asset, liability, equity, income, expense}` — but the **generic CRUD route bypasses it**.
`kernel/metadata.Field` has no `Options`, so an `enum` field is unvalidated, and
`POST /api/v1/account` accepts any string. (The WP-1.5 e2e proves it: it created accounts with
`type: "revenue"`, which is not in the set, and nothing objected.)

P&L and balance sheet classify by exactly that field, so an out-of-set type would silently vanish from
both statements — and both would still *look* balanced while being wrong. Silent wrongness in a
financial statement is the worst possible failure mode.

**Decision, three parts:**
1. Reports classify against the closed set and put anything else in an explicit **`unclassified`**
   bucket that is *rendered*, never dropped. A misconfigured chart of accounts becomes visible instead
   of becoming a quiet misstatement.
2. The balance-sheet reconciliation test asserts `unclassified` is empty for a well-formed tenant, so
   the bucket cannot silently become normal.
3. Fix the WP-1.5 e2e to use `income`, and **flag the root cause** — enum option validation in the
   metadata engine — as work for an ADR-006 WP. It is not fixed here: adding `Options` to `Field` is a
   schema-language change with an overlay story, and doing it inside a reporting WP is how kernels rot.

## 6. Metrics layer v1: definitions as data, evaluated through one path

docs/21 §1 describes ~150 certified metrics with dimensions, comparisons, grains and a formula DSL.
That is a product surface, not a v1, and a formula parser is its own WP.

**Decision:** v1 ships the *shape* and the governance, not the volume — a `Metric` definition
(name, label, unit, `good_direction`, owner module, required permission) bound to a **Go evaluator**
rather than a parsed formula string. Roughly a dozen metrics the reports already compute
(AR outstanding, aging bucket totals, revenue, expenses, net income, DSO). Tenant/plugin-authored
metrics and the formula DSL arrive with the builder (WP-4.13).

Why a registry at all, if it is only a dozen: docs/21's guarantee is *"the CEO's revenue number and the
CFO's revenue number can never disagree"* — one definition, one evaluator, one permission check. That
property is established by the seam existing, and is impossible to retrofit once four screens each
compute revenue their own way.

**Permissions are evaluated in the engine, not the UI** (docs/21 §1, INV-T1): every metric declares an
object+action, `authz.Authorize` runs before evaluation, and the underlying reads are tenant-scoped.
The permission-leak suite is the AC for this.

## 7. Export: hand-rolled CSV and XLSX, no new dependency

CSV is `encoding/csv`. XLSX is a ZIP of XML parts — `archive/zip` + `encoding/xml`, both stdlib.

**Decision:** hand-roll the minimal XLSX writer, following the WP-1.4 precedent where the invoice PDF
writer was hand-rolled rather than taking a dependency. Scope is a single worksheet with a header row,
typed number/string cells, and a frozen header — enough for "open it in Excel and it is right". No
styling engine, no formulas, no multi-sheet.

Rejected: `excelize` (a large dependency needing an ADR, for a feature a few hundred lines of stdlib
covers). If real formatting requirements arrive, that ADR is the right conversation to have then.

## 8. Aging buckets run off issue date, because there is no due date yet

Standard AR aging ages off the *due* date. `Invoice` has `issue_date` and no `due_date` — payment terms
were explicitly deferred by WP-1.4 (decisions §"Deferred").

**Decision:** v1 ages off `issue_date` with the standard `current / 1–30 / 31–60 / 61–90 / 90+`
buckets, and the report labels the basis explicitly so nobody reads it as due-date aging. Payment terms
(and true due-date aging) land with the WP-4 banking/dunning work that owns terms.

## 9. Drill-down: report rows carry source ids

docs/21 §2 wants "drill-down to source document". v1: every report row carries the ids it was computed
from (`account_id`, `entry_id`s for a GL detail, `invoice_id`s for an aging row), so the client can
link to the existing document routes. No new drill-down query engine — the report *is* the query, and
the ids are already in hand when it runs.

## 10. Module boundary

New `modules/reporting`, declaring `requires: [ledger, invoicing]` in its manifest — the WP-1.4
precedent (a module may import a sibling it declares). No cycle: ledger and invoicing do not import
reporting. Receipts live in `modules/invoicing` (AR is its module), so reporting does not need to own
any write path at all — it is read-only by construction, which is the strongest form of the
permission-leak guarantee.

## 11. Invariants

| Invariant | Touched how | Test |
|---|---|---|
| **INV-F8** *(new)* | receipts may not over-apply an invoice | property test: random receipt sequences never exceed gross; concurrent double-apply rejected |
| **INV-E5** | every report reconciles against a fold of the raw log — this *is* the AC | each report computed twice (projection vs. event fold), asserted equal |
| **INV-F1** | receipt GL entry balances | template property test |
| **INV-F5** | receipts reach the GL only via `buildReceiptJournal` | adversarial: no other path writes AR |
| **INV-F2** | posted receipt is immutable | raw UPDATE/DELETE rejected, both dialects |
| **INV-F6** | receipt numbers gapless at acceptance | failed post consumes no number |
| **INV-T1** | reports/metrics must not leak rows the viewer cannot read | permission-leak suite: cross-tenant + under-privileged reader |
| **INV-T2/T4** | receipt posting is authorized and attributable | authz required; audit row written |

## 12. What PR-B actually shipped, against §6's plan

The metrics layer landed as designed: nine certified metrics bound to Go evaluators, each declaring
the object+action the engine checks before evaluating. Two properties are worth naming because they
are what the layer exists for:

- **One number.** `ar_outstanding` and the AR aging report share `OpenItems`, and
  `TestARMetricAgreesWithAgingReport` fails if they ever diverge. Ledger metrics share `Load` with the
  statements, so a dashboard tile and the P&L cannot disagree about revenue.
- **The subledger ties to the GL.** `TestARAgingTiesToLedgerARBalance` asserts AR aging totals equal
  the ledger's AR control-account balance. That is the check an auditor runs first, and nothing else
  in the suite would have caught a drift between them.

`EvaluateAll` **omits** metrics the caller may not see rather than failing the request: a dashboard
should render what the viewer is entitled to and stay silent about the rest. The catalog itself is not
secret — knowing a metric exists is not knowing its value — so `Metrics()` is unfiltered and
evaluation is the only gate.

---

## Deferred, flagged

- Formula DSL, dimensions, comparisons, grains, ~150 metrics → WP-4.13 builder (§6)
- Cash-flow statement, GL detail, invoice registers, tax returns → later reporting WPs (docs/21 §2)
- Report scheduling/delivery, saved-report objects, NL "ask-in-English" → docs/21 §2, WP-5.8
- PDF/Parquet export → this WP does CSV + XLSX only
- Enum option validation in the metadata engine → ADR-006 WP (§5) — **the one finding here that
  outlives this WP**
- Due-date aging + payment terms → Phase 4 banking (§8)
- Payment matching, dunning, partial-payment allocation strategies → Phase 4 (roadmap says so)

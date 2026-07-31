# WP-1.8 — Role dashboard packs v1 — interpretation & decisions

WP-1.8 ([docs/11-ROADMAP.md](../11-ROADMAP.md), [docs/21 §3–4](../21-REPORTING-DASHBOARDS.md)):
role dashboard packs (CEO/CFO/AR/AP) with KPI cards carrying mandatory
comparisons. **AC: fresh tenant shows live role dashboard from seed data**, plus
docs/21's *"5-second-headline heuristic reviewed per pack"*.

Design inputs: docs/21 (§1 metrics layer, §3 dashboards, §4 role packs),
[docs/19](../19-DATA-INTEGRITY.md) (always), [ADR-006](../adr/ADR-006-metadata-customization.md)
(packs are customization data, originals stay pristine), [ADR-018](../adr/ADR-018-composability.md)
(capability gating), [ADR-010](../adr/ADR-010-frontend.md) (no charting dependency
is approved), and the WP-1.6b metrics layer this builds on.

---

## 1. The headline problem: today's comparisons would be fake

docs/21 §3 is categorical: *"KPI cards always render actual vs. target/budget/prior
period with delta and spark-trend — a lone '4.2M' is impossible by default."*

The metrics layer cannot honour that yet. `Scope` carries an `AsOf`, and the AR
metrics respect it (`OpenItems` ages off issue date), but every **ledger** metric
— revenue, expenses, net income, total assets, cash position — evaluates through
`Load` → `ReadTrialBalance`, which is the **all-time** projection and ignores
`AsOf` completely. Rendering "revenue vs prior period" on top of that compares a
cumulative number with itself: every delta would be exactly 0.0 %, forever, and
look authoritative.

That is worse than the bare number docs/21 bans, so this WP makes the metrics
layer period-aware before it renders a single card.

**Grain, on the metric definition.** A comparison is only meaningful if you know
what kind of measure you have:

- **Flow** (revenue, expenses, net income) — movement *within* a period. Prior
  period = the same movement one period earlier.
- **Stock** (cash position, total assets/liabilities, AR outstanding/overdue) —
  a balance *as at* a moment. Prior period = the balance at the previous
  period's end, i.e. cumulative through it.

Treating a stock measure as a flow (or the reverse) is the classic dashboard
lie — "cash this month: €40k" when €40k is the whole balance. `Metric.Grain`
makes it a declared property of the definition, checked by tests, not a habit.

**The period is the accounting grain, not wall-clock time.** Journal entries
carry a `period` code and posting is already gated on that period being open
(INV-F3). Filtering by `RecordedAt` would instead answer "what did the server
write between these timestamps", which drifts from the books the moment anything
is posted late — exactly the mismatch that makes month-end numbers argue with
each other. `ledger.BalancesForPeriods` folds the log with a period predicate;
"prior period" is resolved from the `Period` objects ordered by `start_date`.

`ponytail:` the period fold reads the tenant's log on each dashboard load. The
upgrade path — a period-grained projection beside `ledger_balances`, maintained
by the same cursor — is named at the call site. The all-time path keeps its
existing incremental projection.

## 2. A dashboard pack is data, and it lives in `modules/reporting`

Packs are embedded YAML (`modules/reporting/packs/*.yaml`), loaded through
`go:embed` — the same shape as capability manifests, tax seed packs and
translation packs. No new module and no new capability: a dashboard v1 is a
*read-only composition of metrics*, and `modules/reporting` is already read-only
by construction with the metric registry inside it. Introducing a `dashboards`
capability would gate the endpoint's existence, not what it may show — the
permission that matters is the one each metric already declares.

ADR-006's "shipped as customization packages, originals stay pristine, editable
copies" is the WP-4.13 builder's story. v1 ships the pristine originals only;
nothing here writes, so nothing can diverge from them yet.

## 3. Availability is decided by permission, role names only order the list

docs/21 §4 says "New user logs in → their role's dashboard is simply *there*".
Tenant role names are arbitrary strings (`authz.CreateRole`), so a pack cannot
*depend* on a role called `cfo` existing.

- A pack declares `roles:` (suggested names) **and** its tiles' metrics.
- A pack is **available** when the caller can evaluate its headline metric —
  the permission the metric itself declares (INV-T1, checked in the engine).
- A pack is **suggested** when one of its `roles:` matches a role the caller
  actually holds (`authz.RolesFor`, added here). That only decides which
  dashboard opens by default.

So a viewer who cannot read invoices does not get an AR dashboard rendered with
zeros — the tiles they may not see are **omitted**, exactly as `EvaluateAll`
already does, and a pack whose headline they cannot see is not offered at all.
Zeroing would be a permission leak dressed as a number.

**`Period:read` is the dashboard's entry permission.** Rendering has to resolve
which fiscal period it is reporting on, and that reads the tenant's `Period`
records through the ordinary authorized path. A viewer without the grant is
refused (403) rather than served a dashboard scoped to a calendar they were not
allowed to look up — and rather than reporting reaching around authorization
with a direct table read, which is the kind of shortcut that stops being
noticeable after the second one. Listing the catalog needs no such grant: knowing
a dashboard exists is not knowing what is on it.

## 4. The AP pack ships declared but gated (user decision, 2026-07-31)

The roadmap names CEO/CFO/AR/**AP**, but payables is WP-4.1 — Phase 4. An AP
pack today would render nothing but empty tiles.

`packs/ap.yaml` is authored and reviewed now, declaring `requires: [payables]`,
and the loader keeps it out of the catalog until that capability is enabled —
the ADR-018 precedent already used for the `invoice_automation_only` profile
pending `documents.ocr`. It lights up when WP-4.1 lands, with no roadmap edit and
no pack written under time pressure by a WP that is busy building AP itself.

## 5. Seed data is a product surface: `lasterp demo` (user decision, 2026-07-31)

"Fresh tenant shows live dashboard from seed data" needs data to exist. A test
fixture would make the AC a test-only claim; folding it into `bootstrap` would
couple "provision a tenant" to "fill it with fake invoices" and make it easy to
run against a real one.

`lasterp demo --tenant <id>` builds a small book: a chart of accounts, a handful
of customers, posted invoices across **two** periods (so comparisons have
something to compare) and a receipt. Every write goes through the ordinary
authorized path — `ledger.CreateAccount`, `invoicing.CreateDraft`/`PostInvoice`,
`invoicing.PostReceipt` — so the seeded book satisfies INV-F1/F3/F5/F6 by
construction and INV-X5 ("bulk paths get batching, not bypasses") holds. It
refuses to run against a tenant that already has posted invoices, so it cannot
quietly inflate real books.

## 6. KPI cards carry comparisons, not charts (user decision, 2026-07-31)

A card renders: value, comparison basis, delta (absolute + percentage), and the
metric's `good_direction` for colour. No sparkline in v1.

docs/21 §3 names Apache ECharts, but ADR-010 approves no charting dependency, and
adding one needs an ADR — for a WP whose roadmap line is *"KPI cards with
mandatory comparisons"* and whose charting story is explicitly WP-4.13's ("chart
intelligence"). A hand-rolled SVG sparkline would avoid the dependency but needs
a series API the metrics layer does not have, at N period folds per tile.
Deferred to WP-4.13 with the ADR.

Colour never carries meaning alone: the delta is rendered with its sign and an
arrow glyph, so a colour-blind or monochrome reader loses nothing (WCAG 1.4.1,
and the axe gate does not test for it).

## 7. The 5-second headline heuristic, per pack

docs/21's build plan requires this review. The test: *what does the reader learn
in five seconds, and is it the thing that role is accountable for?*

| Pack | Headline (top-left, dominant) | Verdict |
|---|---|---|
| **CEO** | Revenue (period) vs prior period | The one number a CEO is asked about first. Cash position is the runner-up tile, not the headline: it is a *stock* and reads as "how much is in the bank", which is the CFO's framing. |
| **CFO** | Cash position vs prior period end | A controller's five-second question is solvency, not turnover. Net income and AR outstanding support it. docs/21 wants a 13-week forecast here; forecasting needs AP and a cash-flow model (Phase 4+), so v1's headline is the current position, and the pack says so rather than showing an empty forecast tile. |
| **AR** | AR overdue vs prior period end | Overdue is the actionable half of outstanding — "what do I chase today". Outstanding, open-invoice count and the aging report sit beneath it. |
| **AP** | Due in the next 7 days | Authored, gated on `payables` (§4). Reviewed against the same heuristic so WP-4.1 inherits a considered pack, not a placeholder. |

Every headline is a comparison, not a bare figure — which is the rule §1 exists
to enforce.

## 8. Deliberately not in this WP

- **Targets, budgets and prior-year.** docs/21 wants "actual vs
  target/budget/prior period". There is no budget object and no goals module
  (docs/12, Phase 4), so `target` has nothing to read. `prior_year` needs a
  fiscal calendar deep enough to have one, which a v1 tenant does not; adding it
  is a second `window` lookup when a pack asks for it. v1 compares against the
  prior period only, and every card **names its basis**, so a reader is never
  left guessing which comparison they are looking at.
- **Server-side metric labels per locale.** docs/21 §1 sketches
  `label: {en: …, de: …}` on the metric definition. WP-1.7 settled the opposite
  convention for schema-derived text — the client keys it (`metric.<name>`,
  `dashboard.pack.<name>`) and falls back to the server's own label — so
  dashboards follow that rather than opening a second, competing mechanism.
  Consolidating both onto whichever wins belongs with the UI-descriptor work.
- **The grid, drag-drop, cross-filtering, TV mode, sharing, live feed
  subscription** — all WP-4.13/WP-5.8. v1 renders a pack's declared layout.
  Dashboards do not subscribe to the change feed (Phase 2); each load evaluates
  live, and the card shows the as-of it used rather than implying real-time.
- **Tenant-authored dashboards and pack editing** — needs the write path and the
  overlay story WP-4.13 brings.
- **A reporting-currency setting.** The server keeps requiring an explicit
  `currency` (WP-1.6b: a report silently rendered in the wrong one is worse than
  one that refuses), and the web client asks for **EUR** — a constant with a
  comment, not a guess the server makes. A tenant whose books are in another
  currency sees an empty dashboard until this is chosen properly, which needs a
  tenant-level reporting-currency setting that does not exist yet. Known
  limitation, named here rather than hidden behind a plausible default.
- **Remaining role packs** (Sales, Ops, Compliance) — their metrics belong to
  modules that do not exist yet; WP-5.8 owns them.

## 9. Invariants

No new INV-\* is registered: a dashboard is a read-only composition of measures
that already carry their own guarantees.

| Invariant | How this WP touches it |
|---|---|
| **INV-T1** | A pack renders only tiles whose metric the caller may evaluate; unavailable tiles are omitted, never zeroed. Tested with a reduced-grant actor on both dialects. |
| **INV-T2/T4** | No new write path. The demo seeder writes only through authorized module entry points, attributed to a real actor. |
| **INV-E5** | The period-filtered fold must equal the materialized projection when no period filter is applied — a property test, since the new fold is a second answer to "what is the balance" and two answers must agree. |
| **INV-F1/F3/F5/F6** | Hold for the seeded book by construction (it posts through the pipeline); asserted rather than assumed. |
| **INV-X5** | The seeder is a bulk path with no shortcuts — same validation, same numbering, same period rules. |

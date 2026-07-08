# 21 — Reporting & Dashboards: beautiful by design, no code required

**Founding requirement (Dan, 2026-07-07):** easy report pulling from the books (AR, AP, orders, invoices, compliance…); professional, state-of-the-art dashboards showing real, live statistics; no code required to create or update; readable by anyone; role-based defaults (CEO, CFO, Sales, AP, AR…); any user builds with the data they can access.

Consolidates the reporting/dashboard threads from WP-1.6, docs/12, and WP-5.8 into one design.

## 1. The metrics layer (the reason everything downstream is trustworthy)

Between projections and pixels sits a **semantic metrics layer** — every business measure defined once, as metadata:

```yaml
metric: dso                       # Days Sales Outstanding
label: {en: "Days Sales Outstanding", de: "Forderungslaufzeit"}
formula: (accounts_receivable / rolling_revenue_90d) * 90
grain: [company, currency, month]
dimensions: [customer, region, dimension.*]     # slicing allowed by
comparisons: [prior_period, prior_year, target]
format: {type: number, decimals: 1, suffix: " days"}
good_direction: down
owner: module.invoicing
```

Core modules ship ~150 certified metrics (revenue, gross margin, DSO/DPO, AR/AP aging buckets, cash runway, pipeline conversion, inventory turns, on-time payment rate, close-cycle days…). Tenants and plugins add metrics via overlays (ADR-006) — same governance, same builder. **One definition feeds dashboards, reports, exports, goals (docs/12), the MCP tools, and alerts** — so the CEO's revenue number and the CFO's revenue number can never disagree. Permissions apply at the metric level and flow from RBAC + RLS: a metric only computes over rows the viewer can see; sensitive metrics (payroll cost) carry explicit grants. **The builder cannot leak what the user cannot read — enforced in the query engine (docs/19 INV-T1), not the UI.**

## 2. Reports: every book, one click away

- **Canned, always-current:** trial balance, P&L, balance sheet, cash flow, GL detail, AR/AP aging, open orders, invoice registers, payment runs, inventory valuation, tax/VAT returns, compliance evidence (docs/20), audit trails. Each: parameterized (period, company, dimension), drill-down to source document, export CSV/XLSX/PDF/Parquet, schedulable (email/Slack/webhook delivery), API- and MCP-addressable (`run_report`).
- **Report builder (no code):** pick object(s) → columns, filters, groupings, subtotals via point-and-click; saved reports are shareable metadata objects with the same permission flow.
- **Ask-in-English:** "show me overdue invoices over $10k by region" → agent composes from the metrics layer + guarded query engine (docs/06), returns the live report *and* the reusable definition it built — NL is a builder shortcut, not a dead-end answer.

## 3. Dashboards: state of the art by design

The insight from dashboard research: beauty and readability come from **enforced design discipline**, not decoration. LastERP encodes the discipline so users can't build ugly:

- **Opinionated grid:** guided layouts implementing the evidence-based rules — primary KPI top-left with dominant weight, 6–8 visuals per view (soft cap with "split into linked views" prompt at 9+), trends left, breakdowns right, detail tables bottom. Free-form exists; defaults are the pit of success.
- **Every number gets context automatically:** KPI cards always render actual vs. target/budget/prior period with delta and spark-trend — a lone "4.2M" is impossible by default (comparisons come free from the metric definition).
- **Chart intelligence:** the builder picks the right visualization from the metric's shape (time-grain → line; categorical comparison → horizontal bar; part-to-whole → stacked bar/treemap, pie only ≤5 slices; distribution → histogram) — overridable, but the default is always defensible.
- **Design tokens, not choices:** a curated 5–7 color semantic palette (good/bad/neutral/accents) from the LastERP UI kit, WCAG-AA contrast enforced, consistent number/date locale formatting (docs/17), colorblind-safe by default, automatic dark mode. Tenant theming (logo, brand accents) within the token system — brandable, never breakable.
- **Live by architecture:** widgets subscribe to the change feed (docs/04) — a posted invoice moves the CEO's revenue tile within ~1s, online; offline, dashboards render from the local replica with an as-of timestamp. No refresh buttons, no stale lies: every widget shows data freshness.
- **Interaction:** click any figure → drill to the underlying records (full report → source document); cross-filtering within a dashboard; date-range and dimension scoping in the header; annotations ("price increase went live here") on time axes.
- **Rendering:** Apache ECharts (canvas, production-hardened at Alibaba/Baidu scale, handles large series + real-time updates) wrapped in LastERP-tokened components; visx-based custom primitives only where ECharts can't express a bespoke visual. Perf budget: dashboard interactive < 1s from replica, widget update < 100ms on feed events (docs/09 discipline applies).
- **Sharing & display:** dashboards are metadata objects — share to user/team/role, read-only public links (tenant-policy-gated, watermarked), **TV/kiosk mode** for the office wall, scheduled PDF/PNG snapshots to email/Slack, embed tokens for portals (docs/15 auth).

## 4. Role-based defaults (ship configured, not blank)

Shipped as customization packages — instantly usable, fully editable copies (originals stay pristine for upgrades, ADR-006):

| Role pack | Headline (top-left) | Supporting tiles |
|---|---|---|
| **CEO** | Revenue vs target | Cash position + runway, gross margin trend, AR/AP totals, pipeline, headcount cost, goal progress (docs/12) |
| **CFO / Controller** | Cash position & 13-week forecast | P&L vs budget, DSO/DPO, working capital, close-checklist status, FX exposure, covenant/compliance flags |
| **Sales lead** | Pipeline by stage | Win rate, bookings vs quota, top opportunities, sales cycle, new vs expansion |
| **AR clerk** | Overdue by aging bucket | Today's expected receipts, dunning queue, promise-to-pay calendar, DSO trend, top delinquents |
| **AP clerk** | Due in next 7/14/30 days | Approval queue, early-discount opportunities captured/expiring, payment-run status, DPO |
| **Ops / Inventory** | Stock-outs & below-reorder | Inventory turns, open POs vs receipts, valuation by warehouse, count variance |
| **Compliance officer** | Control status (docs/20) | Open DSARs vs deadline, audit-trail anomalies, access reviews due, integrity sentinel status (docs/19) |

New user logs in → their role's dashboard is simply *there*. Personalizing it is drag, drop, done.

## 5. No-code creation flows (three doors, one result)

1. **Gallery:** clone a role pack or community template, tweak tiles.
2. **Builder:** drag metrics from a searchable, permission-filtered catalog onto the grid; every drop is instantly live data (no preview/publish gap); undo everything.
3. **Say it:** "build me a dashboard tracking cash, overdue AR by region, and this quarter's top deals" → agent (L2 pipeline, docs/13) assembles from certified metrics, user adjusts. Editing is the same three doors — a dashboard is never stale metadata someone's afraid to touch.

Learned personalization (L0/L1): the system may *suggest* tiles ("you check the aging report every morning — add it here?"); it never rearranges anyone's dashboard silently.

## Build plan
- **WP-1.6a (amends WP-1.6) Metrics layer v1** + canned financial reports + drill-down. AC: metric values reconcile with event-fold oracle; permission-leak suite green (gauntlet).
- **WP-1.8 Role packs v1** (CEO/CFO/AR/AP) + KPI cards with mandatory comparisons. AC: fresh tenant → role dashboard live with seed data; 5-second-headline heuristic reviewed per pack.
- **WP-4.13 Dashboard builder GA** (grid + chart intelligence + drag-drop + live feed subscription + sharing/TV mode). AC: non-technical tester builds the AR dashboard from spec in <10 min, no docs; p95 budgets met; axe-core green.
- **WP-5.8 (redefined) Dashboards v2:** NL assembly, annotations, cross-filtering, public links/embeds, remaining role packs, community template gallery.

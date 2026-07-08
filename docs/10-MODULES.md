# 10 — Functional Modules

Each module = Go package under `modules/` + core-layer object schemas + posting templates + MCP tools + seed data + docs. Modules depend on kernel + ledger only (no lateral imports; events for cross-module reactions).

## Composability (every module is optional — see [ADR-018](adr/ADR-018-composability.md))

Use only what you want: each module ships a **capability manifest** (provides/requires/enhances/modes); the kernel solver computes and *shows* the dependency closure before enabling ("Invoicing also enables: Contacts, Ledger Core, Tax Engine"). Disabled modules vanish everywhere (nav, API, MCP, search, sync) but their data is retained; reduced modes are first-class (invoice OCR in capture→export mode needs no ledger; CRM-only needs just contacts). Shipped profiles: Personal · Accounting-only (QuickBooks replacement) · CRM-only · Invoice-automation-only · Services SMB · Product SMB · Full suite.

| Module | Provides | Requires | Notable reduced mode |
|---|---|---|---|
| M1 Ledger | ledger.core, dimensions, periods | — (kernel only) | — |
| M2 Invoicing/AR | invoicing, ar.aging | contacts, ledger.core, tax.engine | — |
| M3 Payables/AP | payables, ap.aging, invoice_capture | contacts, ledger.core, tax.engine | **capture_export**: OCR + validate + export, no books |
| M4 Banking | banking, reconciliation | ledger.core | statement-import-only (no payment initiation) |
| M5 CRM | crm, pipeline | contacts | standalone (no financial deps) |
| M6 Inventory | inventory, stock.movements | contacts | quantity-only (valuation requires ledger.core) |
| M7 Payroll | payroll | hr.core, ledger.core + country pack | — (employee records now live in M10) |
| M10 HR / Employee Directory | hr.core, org.structure | contacts | standalone (no payroll, no ledger needed) |
| M8 Projects & Time | projects, time | contacts | time-tracking-only |
| M9 Work Management | work, docs, goals, forms, dashboards | — (kernel only) | standalone (the ClickUp-only user) |
| Tax engine | tax.engine | — | rates-lookup-only |
| Contacts (shared) | contacts | — | — |

## M1 — Ledger (the heart; built first, hardest gates)
Chart of accounts (templates per country/industry as seed packs), journal entries, dimensions (cost center/project/department), fiscal years & periods with hard close, multi-currency (transaction/base/reporting currency, effective-dated rates, realized/unrealized FX gain-loss routines), reversing & recurring entries, trial balance / P&L / balance sheet / GL detail as projections. **Invariants (storage-enforced):** balanced entries, no posting to closed periods, immutable posted entries, every non-GL document that touches money posts through a template.

## M2 — Invoicing / AR
Customers, quotes → invoices (or via CPQ connector), credit notes, recurring invoices, payment terms & early-payment discounts, tax engine integration (ADR-013), PEPPOL/UBL e-invoice output, payment links (Stripe/GoCardless connectors), receipts & allocation (auto-match suggestions), dunning (rule-driven, AI-draftable letters), AR aging.

## M3 — Payables / AP
Vendors, bill capture (upload → OCR/AI extraction → draft bill for review), 3-way match (PO/receipt/bill) when inventory active, approval workflows by amount/category, payment runs (SEPA/NACHA files or payment-provider connectors, 4-eyes gated), expense claims, AP aging, 1099/DAC7-style vendor reporting hooks.

## M4 — Banking & Reconciliation
Bank accounts, statement import (open-banking connectors + camt.053/MT940/OFX/CSV), rule-based + AI-suggested matching (`reconcile_bank_statement` MCP tool), reconciliation workbench (hand-built UI), cash position dashboard. Full design: [18-BANKING-FINANCIAL-INTEGRATION.md](18-BANKING-FINANCIAL-INTEGRATION.md).

## M5 — CRM (deliberately lean)
Leads → opportunities (pipeline as configurable workflow), activities (calls/meetings/emails via Graph/Gmail connectors), campaigns (basic), conversion → customer + quote. Deep CRM needs → Twenty/Salesforce/HubSpot connectors rather than feature-chasing.

## M6 — Inventory
Items (stocked/non-stocked/service, variants), warehouses & locations, movements (receipt, issue, transfer, adjustment, count — event-sourced), valuation (moving average + FIFO) posting to GL, reorder points → purchase suggestions, offline-friendly warehouse ops (count/pick queue as offline-writable commands).

## M7 — Payroll (last core module; hardest compliance surface)
Builds on M10 HR (employees/contracts live there): pay components (earnings/deductions/employer costs as metadata), payroll run lifecycle (draft → calculated → approved [gated] → posted → paid), GL posting templates, payslip generation.
**Country packs as plugins** (per ADR-013): each pack = calculation rules + withholding tables (effective-dated data) + statutory report formats + filing calendar. Launch order: US (federal + state via PayrollTax API adapter or tables), UK, then community. Payroll ships behind an "understand the compliance burden" enablement gate per country.

## M8 — Projects & Time (v1.x)
Projects as dimension + structure, time entries (offline-writable), billable rollup → invoicing.

## M9 — Work Management (the ClickUp replacement; see [12-WORK-MANAGEMENT.md](12-WORK-MANAGEMENT.md))
Tasks/projects (list/board/gantt/calendar), docs & wikis, goals bound to live ledger measures, forms/intake, dashboards, notifications inbox, time tracking. The differentiator: work items link typed references to any ERP object — one graph, one permission model, one agent context. Deliberately excludes full chat/whiteboards/email (integrate instead).

## M10 — HR / Employee Directory (standalone; payroll NOT required)
Employee tracking without touching pay: **Employee** (employee number via NumberSeries, job title, department/team, employment type & status, hire/termination dates with full history, location, manager), **org structure** (manager tree → live org chart; departments as a dimension so costs roll up when ledger is on), **Contract/position history** (effective-dated — promotions and title changes are records, not overwrites), documents (signed contracts, reviews — versioned attachments), **certifications & compliance items with expiry dates** (expiring cert → automatic work item, docs/12), onboarding/offboarding as templated work-management projects. v1.x: basic time-off tracking (types, balances, approval via org tree).

**Employee ↔ User link (optional, per Dan's requirement):** an Employee may be linked 1:1 to a system User — enabling self-service (view/edit own record per field permissions, own payslips when payroll is on, own time entries), manager rollups, and approval routing up the org tree. Fully optional in both directions: warehouse staff can be employees without logins; contractors/accountants can be users without employee records. Linking/unlinking is audited; deactivating a user never deletes the employee record (and vice versa).

**Privacy posture:** HR objects are PII-dense by default — field-level masks on sensitive fields (salary band, birth date, IDs) per docs/08, full DSAR/retention coverage per docs/20; HR admins see what their role grants, not everything.

**Composability (ADR-018):** requires only `contacts`. Payroll requires `hr.core` (+ ledger + country pack). Enhance bridges: with `projects` → assignment/capacity views; with `ledger.core` → department cost dimension; approval flows use the org tree whenever `hr.core` is enabled.

## Cross-module services (kernel)
Documents/attachments with versioning; PDF rendering (invoices/payslips) via template packs; notification center (in-app/email/Slack/Teams); saved reports & scheduled report delivery; import/export center.

## Build order rationale
Ledger first because everything posts into it; AR before AP (cash in beats cash out for early adopters); banking early because reconciliation is the #1 daily pain QuickBooks refugees cite; payroll last because a compliance mistake there is existential.

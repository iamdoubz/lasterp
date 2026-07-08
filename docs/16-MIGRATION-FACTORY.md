# 16 — Migration Factory: AI-assisted escape from SAP, IFS, NetSuite & friends

Migration is LastERP's primary growth weapon: nobody adopts an ERP into a vacuum. The Migration Factory is a **product**, not a consulting practice — an agent-driven pipeline that turns "we're trapped in SAP" into a validated, reversible cutover. The self-evolution machinery (docs/13) does the heavy lifting: shadow tenants, agent pipelines, approval gates.

## Pipeline (five stages, each resumable and audited)

### 1. Extract
Get source data out via the least-privileged path available:
- **SAP ECC/S/4HANA:** OData services, RFC table reads (BKPF/BSEG financial docs, KNA1/LFA1 masters), or plain SE16/table CSV dumps when API access is politically impossible. IDoc archives for history.
- **NetSuite:** SuiteAnalytics Connect (ODBC), saved-search CSV exports, SuiteTalk; NetSuite's own MCP connector where enabled.
- **IFS:** Oracle views / IFS Cloud REST (Applications 10+ / Cloud APIs).
- **QuickBooks:** QBO API + QBXML; **Odoo/ERPNext:** direct Postgres/MariaDB reads; **Lotus Notes/Domino:** DXL export.
- Fallback for anything: **file-drop mode** — CSVs/Excel/database dumps into the staging area. If it tabulates, we can migrate it.
Extraction lands in a **staging lake** (DuckDB/Parquet in the migration workspace) — raw, immutable, hash-verified. Nothing touches the LastERP tenant yet.

### 2. Understand (AI: schema & semantics inference)
Agent profiles every staged table/field: types, cardinality, null rates, value distributions, FK inference, unit/currency detection. LLM pass interprets *semantics* — "KTOKD is a customer account group; maps to customer category" — using a shipped knowledge pack per source system (curated mappings of SAP/NetSuite/IFS/QuickBooks schemas: field meanings, quirks, known dirty-data patterns; community-extendable like tax packs). Output: a **source model** document the customer's team can read and correct.

### 3. Map (AI proposes, human disposes)
Agent generates a **mapping plan**: source model → LastERP objects (docs/03), including CoA restructuring proposals, custom-field creation where the source has no LastERP equivalent (via L2 customization packages — the mapping *builds the tenant's schema*), value transformations (LLM-normalized descriptions, unit conversions, deduplication candidates with evidence), and open-balance strategy (see below). Every mapping row carries: confidence score, sample before/after, rationale. Review UI = spreadsheet-like triage sorted by confidence ascending; SMEs correct in plain language, agent propagates corrections pipeline-wide. Mapping plans are versioned artifacts — exportable, diffable, reusable across similar migrations (the flywheel: every migration improves the knowledge packs).

### 4. Validate (the stage that earns trust)
Runs against a **shadow tenant** (WP-6.3 machinery), repeatable until green:
- **Financial reconciliation is arithmetic, not vibes:** trial balance per period source vs. migrated must match to the cent; AR/AP open-item totals per counterparty match; inventory quantity × valuation match; payroll YTD match. Discrepancy report drills to record level.
- Structural checks: referential integrity, orphan detection, duplicate audit, sequence continuity.
- **Golden-document sampling:** N documents per type rendered side-by-side (source screenshot/report vs. LastERP) for human sign-off.
- Row-count + checksum manifest for every entity, kept as the migration certificate.

### 5. Cut over (reversible by design)
- **History strategy (tenant choice):** (a) *open balances + opening journal* — clean start, history retained read-only in the staging lake, searchable via a "legacy archive" MCP tool; (b) *full history replay* — historical documents imported as posted events with original dates in locked periods (auditors love it, takes longer); (c) hybrid — N years replayed, older as balances + archive.
- **Parallel run mode:** LastERP ingests deltas from the source (via the same connectors, docs/07) for 1–2 periods; a daily reconciliation agent compares both systems' trial balances and flags drift — cutover confidence measured, not asserted.
- Cutover checklist is a system-templated work-management project (docs/12) with gated steps; final delta sync → source goes read-only → go-live. **Rollback:** until the go-live gate, deleting the tenant data and re-running is one command; the staging lake and mapping plan are never consumed.

## Product surfaces
- `lasterp migrate` CLI + Migration Workbench UI (staging browser, mapping triage, reconciliation dashboards).
- Migration MCP tools — the entire pipeline is agent-drivable with the same approval gates (a Claude Code session can run a migration).
- Knowledge packs as versioned data in-repo: `migrations/packs/{sap-ecc,s4hana,netsuite,ifs,quickbooks,odoo,erpnext,dynamics-bc,sage,xero,notes-domino}/`.
- Free assessment mode: point extract at the source, get a scoping report (entity counts, complexity flags, custom-field inventory, estimated effort) — zero commitment; this is the top-of-funnel.

## Honest constraints
- Customizations don't migrate — *requirements* do: agent inventories source customizations (SAP Z-tables/user exits, NetSuite scripts) and proposes LastERP-native equivalents (L2 metadata or L3 plugin scaffolds) as a report; a human decides what still matters (most of it won't — that's a feature).
- LLM mapping is a proposal engine, never an unreviewed writer: everything enters LastERP through the standard command pipeline with validation; reconciliation is deterministic arithmetic, not model judgment.
- Source-system licensing/ToS for extraction is the customer's call; file-drop mode always works.

## Build plan
- **WP-7.1** Staging lake + file-drop extract + profiler. AC: 1M-row CSV set profiled <10 min; manifest checksums stable.
- **WP-7.2** Mapping engine + review UI + L2-package emission. AC: QuickBooks pack maps a real QBO export end-to-end with >90% auto-accepted rows.
- **WP-7.3** Reconciliation suite. AC: trial-balance match to the cent on seeded corpus incl. adversarial dirty data; drill-to-record works.
- **WP-7.4** QuickBooks + ERPNext + Odoo packs GA (absorbs WP-4.6). **WP-7.5** NetSuite + Dynamics BC packs + parallel-run agent. **WP-7.6** SAP + IFS packs + customization-inventory agent. **WP-7.7** Assessment mode + Notes/Domino (absorbs WP-5.5).
- Sequencing: WP-7.1–7.4 land with Phase 4 (SMB sources first — largest volume, simplest schemas); 7.5–7.7 with Phase 5+.

# 14 — Where Every Other System Fails (and LastERP's Answer)

The pain-point matrix that drives design priorities. Sources: user review mining (Capterra/G2), implementation-failure research, vendor comparison studies (2026).

## Cross-vendor pain points

| # | Pain (who suffers) | Root cause in incumbents | LastERP answer |
|---|---|---|---|
| 1 | **Slow.** "NetSuite is slow today" is a weekly ritual; customization weight degrades performance every quarter | Server-rendered UIs, shared clouds, customization piled on hot paths | Local SQLite replica = reads never touch the network; performance budgets in CI (docs/09); plugin latency budgets enforced by sandbox |
| 2 | **Can't find anything.** Data in system, invisible to users; navigation takes 7 clicks | Search bolted on, per-module silos | Kernel-level FTS + semantic search across every object incl. custom ones; global command palette; ask-in-plain-language via MCP agent (docs/06) |
| 3 | **Reporting requires consultants.** Every new question = a change request | Report builders divorced from data model; SQL access forbidden | Metadata-aware report engine + NL→guarded-SQL agent tool; every report exportable/API-accessible; users iterate themselves |
| 4 | **Implementation costs 1–5× license; 3–12 months** | Consultant-dependent configuration, no sane defaults | Ship configured: country seed packs (CoA, tax, document templates), guided setup interview (AI-assisted), importers as products (docs/07 L2.9); solo mode live in 10 min |
| 5 | **Upgrade hell** (SAP's defining disease) | Customization patches core | Overlay customization survives upgrades by construction (ADR-006); pre-upgrade compatibility report |
| 6 | **Vendor lock-in + price ratchets** (QuickBooks, NetSuite renewals) | Proprietary data formats, hostage pricing | AGPL, full-fidelity export always, self-host first-class; leaving must be easy for staying to mean anything |
| 7 | **Rigid workflows** (IFS): system dictates process | Hardcoded processes | Workflows are metadata: states/transitions/approvals tenant-editable within invariants |
| 8 | **Steep learning curve, ugly UX** — universal complaint | UX budget spent on feature checklists | Keyboard-first, sub-100ms UI; role-based home screens; every screen has "explain this" agent affordance |
| 9 | **Integration gaps → data silos → swivel-chair work** | Per-integration consulting projects | Connector framework + universal surfaces (docs/07); MCP makes LastERP legible to any agent stack |
| 10 | **The ERP/work-management split** (the ClickUp gap, see below) | ERP holds records; work happens in ClickUp/Asana/Notion/email; context dies between them | M9 Work Management (docs/12): tasks/docs/goals native, linked to ERP objects — the collections task links the actual overdue invoice |
| 11 | **Offline = outage.** Warehouse/field work stops with connectivity | Cloud-only architecture | Offline-first is the architecture, not a feature (docs/04) |
| 12 | **Reliability trust** (QuickBooks outages, silent data corruption) | Opaque SaaS, weak audit | RPO-0 acknowledged writes, immutable trails, self-healing runtime + data sentinels (docs/13) |
| 13 | **AI theater.** Copilots that summarize but can't act, or act without governance | AI bolted onto closed cores | Agents as governed principals with real tools + approval gates (docs/06); self-evolution loop (docs/13) |
| 14 | **Per-seat pricing punishes adoption** (ClickUp/NetSuite: every viewer is a license) | Business model, not tech | Free software; hosted pricing (if any) never per-seat-punitive; unlimited read-only/portal users forever |

## The ClickUp lesson (what to steal, what to avoid)

**Steal:** everything-in-one-context conviction; agents with team memory that get more useful with use; connected/enterprise search as a headline feature; ambient suggestions before you ask; template + import ecosystem as growth engine; free tier generosity.

**Avoid:** sprawl without a system of record underneath (ClickUp tasks reference money but hold no financial truth — LastERP inverts this: the ledger is the ground truth and work items attach to it); AI credits/seat-gating; 718KB-bundle-style bloat — our perf budgets apply to work management too; "replaces all software" without depth in any regulated domain.

**LastERP's structural advantage over ClickUp:** an agent inside ClickUp can tell you a task is late. An agent inside LastERP can tell you the task is late, that it blocks a $40k invoice, which customer's credit standing it affects, and draft the dunning letter — because tasks, ledger, CRM, and documents share one object graph, one permission model, one audit trail.

## Design priorities derived (ranked)
1. Speed and search are retention features — kernel-level, never module afterthoughts.
2. Time-to-first-value < 1 day self-serve (setup interview + seed packs + importers).
3. Work management ships in v1.x, not "later" — it's the daily-active surface that makes the ERP sticky.
4. Every complaint above becomes a standing regression test category in CI where testable (perf, search recall, setup time).

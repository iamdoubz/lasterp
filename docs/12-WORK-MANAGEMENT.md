# 12 — Work Management (Module M9: the ClickUp replacement)

ERP holds the records; work happens somewhere else — that split is where context dies (ClickUp's own research: "60% of work is lost in context"). LastERP closes it by making work items native objects in the same graph as invoices, customers, and journal entries.

## Scope (deliberate)

**In:** Tasks & projects (list/board/gantt/calendar views), docs & wikis, goals/OKRs, forms/intake, dashboards, comments & @mentions, notifications inbox, time tracking (feeds Projects & Time M8 → billing), templates, checklists, recurring tasks, guest/portal access.
**Out (integrate, don't build):** full chat (Slack/Teams connectors + comment threads suffice), whiteboards, video clips, email client. The moment we chase ClickUp's 100-feature wall we inherit their sprawl; we win on depth of linkage, not breadth of toys.

## Objects (standard metadata objects — everything in ADR-006 applies)

- `WorkItem` (CRUD): type (task/bug/approval/checklist-item via tenant-extensible enum), state (workflow metadata), assignees, dates, priority, estimates, tags, parent/subtask tree, dependencies, **`links[]` — typed references to any object** (Invoice, Customer, PayrollRun, Document…), custom fields via overlays.
- `Project`: container + rollups; can bind to the M8 project dimension so work rolls into GL project costing.
- `Doc` (CRUD, versioned): rich-text blocks, backlinks, embeds of live object views (a doc can embed the live AR aging for customer X). Prose fields are first candidates for Yjs CRDT (WP-5.6).
- `Goal`: target metric bound to a **live system measure** (report expression, e.g. "Q3 revenue" reads the ledger — OKRs that update themselves) or manual.
- `Form`: public/internal intake → creates any object (lead, expense claim, work item) through the standard command pipeline.
- `Dashboard`: grid of report blocks/widgets (same report engine + UI slots from docs/05).

## The linkage rule (what makes this not-another-task-app)

Every financially meaningful flow can emit work: dunning generates collection tasks linked to the invoice; month-end close is a system-templated project whose checklist items are gated MCP actions; a rejected sync command files a review task; bill approval IS a work item. Conversely, work rolls into money: time entries → billable invoices, project tasks → project cost dimension. One graph, one permission model, one search index, one agent context.

## Offline & sync
Work items, docs, comments, time entries are offline-writable (naturally commutative; per docs/04 conflict classes). This makes LastERP's work management *more* capable offline than ClickUp is online.

## Agents in work management (the ClickUp Brain answer, governed)
Auto-triage intake forms; standup/status digests from actual activity (not self-reporting); "assign agent" suggestions from workload + history; goal drift alerts ("at current AR collection rate, Q3 cash goal misses by 12%") — every one through MCP tools with the caller's permissions, per docs/06. Team-memory features come from the learning loop in docs/13, not a separate AI silo.

## Build plan
- **WP-4.8** WorkItem/Project/views + linkage + notifications inbox. AC: task lifecycle e2e; link-to-invoice flow; offline create/edit via sim harness.
- **WP-4.9** Docs + embeds + backlinks. AC: doc with live report embed renders offline from replica.
- **WP-4.10** Forms + intake routing; Goals bound to live measures. AC: form → lead → task chain e2e; goal reads ledger projection.
- **WP-5.8** Dashboards v2 + time tracking → billing bridge. AC: tracked time invoiced end-to-end.

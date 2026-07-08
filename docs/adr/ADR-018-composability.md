# ADR-018: Modular composability — capability graph, profiles, use-what-you-want

**Status:** Accepted · 2026-07-07 · Requirement: Dan — "if they just want the CRM, go for it; the system must know what core it needs; modular and composable by default, simple defaults per business size."

## Prior art (researched)
- **Odoo (the model done best):** every app is a module with declared `depends`; installing an app auto-installs its chain; `auto_install` bridge modules activate glue when both sides are present. Weakness: module sprawl + upgrade fragility across module boundaries.
- **NetSuite:** "suites"/module licensing — coarse commercial bundles, not technical composability; you pay per module, everything ships entangled.
- **Dynamics 365:** separate apps (Sales, Finance, SCM) over shared Dataverse — real composability, but apps feel like different products stitched together.

LastERP takes Odoo's dependency-graph idea, backs it with one kernel (no stitching), and makes it free.

## Decision

**1. Kernel vs modules.** The kernel (identity/tenancy, metadata engine, command pipeline, event store, audit, sync, search, files, notifications, approval service — the "OS") is always on and is not disableable. Everything in docs/10 (M1–M9) is a module.

**2. Capability manifest per module.** Modules declare, as metadata:
```yaml
module: invoicing
provides: [invoicing, ar.aging, einvoicing.output]
requires: [contacts, ledger.core, tax.engine]        # hard deps
enhances: [{when: inventory, adds: stock-aware-invoicing}]  # Odoo-style bridges,
                                                            # auto-active when both sides on
modes: [{name: capture_export, requires: [contacts, documents.ocr]}]  # reduced modes
```
Capabilities are finer than modules (`contacts`, `ledger.core`, `documents.ocr`), so light dependencies don't drag whole modules in.

**3. The solver (the logic Dan asked for, built into the kernel).** Enabling a module computes the dependency closure and shows it before applying: *"Enabling Invoicing will also enable: Contacts, Ledger Core, Tax Engine."* Transparent, never silent. Disabling checks reverse dependencies the same way. The solver is deterministic metadata evaluation — no hidden coupling.

**4. Reduced modes are designed, not accidental.** Want only OCR/invoice-capture automation? `invoice_capture` in `capture_export` mode needs no ledger — extract, validate, export to whatever system you keep books in. Turn on `ledger.core` later and the same module upgrades to full AP posting. CRM-only needs just `contacts`. Each shipped reduced mode is a tested configuration, listed in the module's docs.

**5. Disabled = invisible and inert, never destructive.**
- Hidden from: navigation, permissions UI, MCP tool catalog (scope-filtered anyway), search results, sync scopes, dashboards/metrics catalog.
- API: endpoints return RFC-7807 `capability-disabled` (explicit, not a confusing 403/404).
- Cross-module event subscriptions no-op cleanly when the target is off.
- **Data is retained on disable** (disable ≠ delete); re-enable restores everything. Purge is a separate, explicit, audited admin action (respecting retention law, docs/20).

**6. Profiles = curated enable-sets + role packs + seed data.** Shipped: **Personal** (invoicing + expenses), **Accounting-only** (the QuickBooks replacement: ledger, AR/AP, banking), **CRM-only**, **Invoice-automation-only** (capture→export), **Services SMB**, **Product SMB** (+inventory), **Full suite**; setup interview (docs/14 §priorities) recommends one from company type + the docs/22 size tiers. A profile is a starting point — every switch stays user-flippable afterward.

**7. Integrity floor.** The solver cannot disable kernel services; any module touching money requires `ledger.core` (INV-F5 makes bypass impossible anyway); the Integrity Gauntlet gains a composability suite: every shipped profile + every reduced mode boots, passes smoke, and cleanly handles events destined for disabled modules.

## Consequences
- Module boundaries already enforced (no lateral imports, events between modules — CLAUDE.md) are what make this cheap; the manifest formalizes what the architecture required anyway.
- Docs/10 gains a provides/requires table per module; new modules must declare manifests or fail CI.
- Build: **WP-0.9 capability registry + solver** (Phase 0 — shapes module structure from the first module) · profile presets land with WP-1.8 role packs · composability gauntlet suite with WP-0.8's framework.
- Honest limit: cross-module *financial* consistency means some combos are refused, with the reason shown (e.g., inventory valuation without ledger.core runs quantity-only mode — valuations need books).

# 00 — Vision

## Product thesis

Every incumbent ERP fails on at least one axis LastERP treats as non-negotiable:

| Axis | SAP | IFS | QuickBooks | Odoo | ERPNext | **LastERP** |
|---|---|---|---|---|---|---|
| Free & fully open source | ✗ | ✗ | ✗ | ✗ (open core) | ✓ | ✓ |
| Fast (sub-100ms UI) | ✗ | ✗ | ✗ | ~ | ~ | ✓ |
| Offline-first w/ sync | ✗ | ✗ | ✗ | ✗ | ✗ | ✓ |
| Customization survives upgrades | ✗ | ✗ | n/a | ~ | ~ | ✓ |
| Sandboxed user plugins | ✗ | ✗ | ✗ | ✗ | ✗ | ✓ |
| AI-native (MCP) | bolt-on | bolt-on | bolt-on | bolt-on | bolt-on | ✓ |
| 5 → 50k users, same codebase | ✗ | ✗ | ✗ | ~ | ✗ | ✓ |

## Lessons we are building against

From researching why ERPs fail (75% of implementations derail; avg failed project costs $10.6M — NetSuite/TechTarget research):

1. **Upgrade hell from customization.** Incumbents let customers patch core behavior; every upgrade then breaks. → LastERP: customization is metadata + sandboxed plugins with stable APIs; core is never patched.
2. **Frappe/ERPNext's ceiling:** metadata-driven DocTypes are the right idea, but no native horizontal scaling, jQuery-era UI, submit-and-lock workflows that fight real processes. → Keep the metadata idea, fix the runtime.
3. **Odoo's trap:** open core — the good parts are paywalled. → Everything in LastERP is open; sustainability comes from hosting/support, never feature gates.
4. **Tryton's gift:** database-level enforcement of double-entry integrity, no backdating posted entries without reversal. → Adopt wholesale.
5. **Lotus Notes' gift:** true offline replicas with sync that people relied on for decades. → Rebuild with modern primitives (SQLite replica + server-authoritative event log), not document soup.

## Principles

- **Local-first, cloud-native.** One codebase serves a laptop in a warehouse with no signal and a 50k-seat cloud tenant.
- **Boring core, wild edges.** The kernel (ledger, sync, authz, metadata) is conservative and heavily tested. Innovation happens in plugins and modules.
- **AI-first means AI-operable.** Every function an accountant can perform, an agent can perform — with the same permissions, approvals, and audit trail.
- **Data is the customer's.** Full export at any time, self-hosting is first-class, no telemetry without opt-in.

## Non-goals (v1)

- Not an e-commerce storefront, MES, or WMS (integrate instead; plugins can add later).
- Not multi-ledger consolidation across 100+ legal entities (design for it, ship later).
- Not a no-code app builder for arbitrary apps — metadata customization serves ERP use cases.
- No blockchain.

## Success criteria

- A 5-person company self-hosts on a $10 VPS or laptop in < 10 minutes (single binary + SQLite mode).
- A 50,000-concurrent-user tenant runs on commodity Kubernetes + Postgres with zero data loss (RPO 0 for acknowledged writes).
- A developer ships a working plugin (any language → WASM) in an afternoon.
- An AI agent closes the books month-end with human approval gates, end to end.

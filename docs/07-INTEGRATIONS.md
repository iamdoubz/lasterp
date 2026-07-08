# 07 — Integrations & Connector Framework

"Integrates with anything" is achieved by layering, not by shipping 500 bespoke integrations.

## Layer 0 — Universal surfaces (free integration with everything)
- Versioned REST API with OpenAPI spec (auto-importable into any iPaaS: Zapier, n8n, Make, Workato).
- Webhooks (HMAC-signed) for every domain event.
- **MCP server** — any AI agent stack integrates instantly.
- Bulk import/export: CSV/XLSX/JSON/Parquet with mapping templates; full-tenant export always available.
- Standard formats where they exist: UBL/PEPPOL e-invoicing, camt.053/MT940/OFX bank statements, SEPA/NACHA payment files, iCal.

## Layer 1 — Connector framework (kernel service)
A `Connector` is a manifest + optional WASM transforms:

```yaml
connector: salesforce
auth: oauth2 {authorize_url, token_url, scopes}
direction: bidirectional
entities:
  - remote: Account        local: Customer
    key_map: {remote: Id, local: external_ids.salesforce}
    field_map: {Name: name, BillingAddress: billing_address, ...}
    transform: ./transforms/account.wasm     # optional, for non-trivial mapping
    conflict: local_wins | remote_wins | newest | review_queue
sync: {mode: incremental, cursor: SystemModstamp, interval: 5m, backfill: paged}
rate_limits: {rps: 5, burst: 20}
```

Framework provides (connectors never reimplement): OAuth2/API-key credential vault, scheduling, incremental cursors, retries with backoff + DLQ, rate limiting, pagination helpers, entity mapping store (`external_ids` on every object), review queue for conflicted records, per-connection health dashboard, structured sync logs.

## Layer 2 — First-party connectors (priority order)
1. **Email/Calendar:** Microsoft 365/Outlook (Graph API), Google Workspace — mail-to-CRM, calendar activities, send documents.
2. **CRM:** Salesforce, Twenty (open-source CRM, natural ally), HubSpot.
3. **CPQ/Sales:** SAP CPQ (quote → order → invoice handoff).
4. **ITSM:** ServiceNow (tickets ↔ projects/assets).
5. **Banks:** open-banking aggregators (Plaid/GoCardless/Nordigen-style) + file import (camt/MT940/OFX) as the always-works fallback.
6. **Tax/compliance data:** Avalara, Vertex, PayrollTax API (per ADR-013).
7. **Payments:** Stripe, GoCardless, Adyen (invoice payment links, payout reconciliation).
8. **Files:** S3/SharePoint/Drive for document archival.
9. **Legacy migration** — superseded by the full AI-assisted Migration Factory: see [16-MIGRATION-FACTORY.md](16-MIGRATION-FACTORY.md) (QuickBooks, Odoo, ERPNext, NetSuite, Dynamics BC, SAP, IFS, Lotus Notes/Domino).
10. **Messaging:** Slack/Teams notifications + approval actions.

## Rules
- Connectors run with a service principal + least-privilege role; every written record is audited like any other actor's.
- Remote systems are never trusted with invariants: inbound data goes through the same command pipeline (a Salesforce sync cannot create an unbalanced journal).
- Connector failures are visible, not silent: health status on dashboard, alert after N consecutive failures.
- Community connectors: same framework, distributed as plugins; certification program for the registry.

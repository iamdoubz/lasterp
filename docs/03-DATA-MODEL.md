# 03 — Data Model

## Layer 1: The metadata object system

Everything below is expressed as `Object` schema documents (ADR-006). Example definition (YAML form; stored as versioned JSON):

```yaml
object: Invoice
module: invoicing
persistence: event_sourced          # or: crud
naming: series                      # INV-{FY}-{#####}, assigned at server acceptance
fields:
  - {name: customer, type: link, target: Customer, required: true, index: true}
  - {name: issue_date, type: date, required: true, default: today}
  - {name: currency, type: currency, default: tenant.base_currency}
  - {name: lines, type: table, target: InvoiceLine, min_rows: 1}
  - {name: total, type: money, computed: "sum(lines.amount)"}
  - {name: tax_total, type: money, computed: tax_engine}
workflow:
  states: [draft, submitted, posted, paid, cancelled]
  transitions:
    - {from: draft, to: posted, action: post, permission: invoicing.post,
       effects: [assign_number, create_gl_entries], requires: [valid_tax, open_period]}
    - {from: posted, to: cancelled, action: cancel, effects: [create_reversal]}
permissions:
  read: [role: sales.viewer]
  write_draft: [role: sales.user]
  post: [role: accounting.poster]
sync_scope: {default: role_based, offline_writable: [draft]}
ai:
  tools: [create_draft, search, explain_balance]
  requires_approval: [post, cancel]
```

From one definition the engine generates: DDL + migrations, Go structs + validation, REST/gRPC endpoints, TS types, UI descriptors, permission matrix, MCP tools, sync scope rules.

### Field types (kernel-defined, closed set — plugins compose, don't invent)
`text, long_text, rich_text, int, decimal, money {amount, currency}, currency, date, datetime (UTC + tz), bool, enum, link (FK), table (child rows), json, file, email, phone, address (structured), duration, percent, computed`

**Money is a first-class type:** integer minor units + ISO-4217 currency, never floats. Decimal math everywhere (shopspring/decimal or int64 minor units; WP-1.1 decides).

## Layer 2: Kernel tables (fixed, hand-designed)

```sql
-- Event store (financial truth; ADR-003)
events(id bigserial, tenant_id uuid, stream_id text, version int,
       type text, payload jsonb, actor_id uuid, command_id uuid UNIQUE,
       occurred_at timestamptz, recorded_at timestamptz,
       UNIQUE(tenant_id, stream_id, version))

-- Sync feed cursors, per client device
sync_clients(id, tenant_id, user_id, device_info, scopes jsonb, cursor bigint, last_seen)

-- Commands (idempotency + offline replay; ADR-004/009)
commands(command_id uuid PK, tenant_id, actor_id, type, payload jsonb,
         status enum(accepted,rejected), result jsonb, received_at)

-- Audit log for CRUD objects (kernel-enforced)
audit_log(id, tenant_id, object, record_id, action, changes jsonb, actor_id, at)

-- Identity & access
tenants, users, service_principals (incl. AI agents), roles, role_grants,
api_tokens, sessions, approval_requests

-- Metadata
object_schemas(name, layer enum(core,module,plugin,tenant), version, definition jsonb, checksum)
customization_packages(...)

-- Plugins & integrations
plugins(id, version, manifest jsonb, capabilities_granted jsonb, status)
connections(id, connector, config jsonb, secrets_ref, status)
webhooks(id, url, events, secret_ref, status)
embeddings(tenant_id, object, record_id, chunk, vector vector(1024))
```

## Layer 3: Core business objects (shipped as core-layer schemas)

**Foundation:** Tenant/Company (multi-company per tenant), FiscalYear/Period (with hard close), Currency + FxRate (effective-dated), Address, Contact, UnitOfMeasure, TaxJurisdiction/TaxRule/TaxRate (effective-dated, ADR-013), NumberSeries.

**Ledger (event-sourced):** Account (chart of accounts, tree), JournalEntry → JournalLine (every line: account, debit XOR credit, money, dimensions). **Invariant enforced at storage layer: Σdebits = Σcredits per entry; no entry in a closed period; posted entries immutable — corrections reverse.** Dimensions (cost center, project, department) as configurable analytic tags on every line.

**AR/Invoicing:** Customer, Invoice, CreditNote, PaymentReceived, PaymentAllocation, DunningRule.
**AP:** Vendor/Supplier, Bill, PaymentMade, PaymentRun (approval-gated batch), ExpenseClaim.
**Banking:** BankAccount, BankTransaction (imported), ReconciliationMatch.
**CRM:** Lead, Opportunity (pipeline stages as workflow), Activity, Campaign — deliberately simple; deep CRM via Twenty/Salesforce connectors (07-INTEGRATIONS.md).
**Inventory:** Item, Warehouse/Location, StockMovement (event-sourced), StockLevel (projection), moving-average + FIFO valuation feeding GL.
**HR (M10, standalone):** Employee (number series, title, department, status, effective-dated position history, optional 1:1 link to User), OrgUnit/Department, Certification (with expiry), employment documents. PII field masks by default.
**Payroll (M7, requires hr.core):** PayComponent, PayrollRun (event-sourced, approval-gated), country rule packs (10-MODULES.md).

Posting rules: every financially-relevant document declares its GL posting template as metadata (e.g., Invoice.post → DR Receivables / CR Revenue / CR Tax Payable) — tenant-adjustable per ADR-006 overlay rules, invariants intact.

## Conventions
- PKs: UUIDv7. All timestamps UTC. Soft-delete only for CRUD objects (`archived_at`); event-sourced objects are cancelled/reversed, never deleted.
- Every table: `tenant_id` first in every index (ADR-005).
- All schema evolution through metadata migrations: expand → backfill → contract; no destructive DDL within a major version.

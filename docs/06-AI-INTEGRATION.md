# 06 — AI Integration

Decision record: [ADR-008](adr/ADR-008-ai-first.md). AI is a governed principal, not a chat box glued on.

## Surfaces

1. **MCP server (kernel).** `lasterp mcp` speaks MCP over stdio (local) and Streamable HTTP (remote); transport/auth details in [15-API-DEVELOPER-PLATFORM.md §4](15-API-DEVELOPER-PLATFORM.md). Tools are auto-generated per object from metadata (`search_invoices`, `create_invoice_draft`, `transition_invoice`, …) plus module-authored task tools:
   - ledger: `explain_account_balance`, `suggest_journal_for_document`, `close_period_checklist`
   - AR/AP: `reconcile_bank_statement`, `match_payments`, `generate_dunning_letters`, `detect_duplicate_vendors`
   - reporting: `run_report(nl_query)` → SQL against projections via a guarded semantic layer (read-only role, row limits, cost caps)
   Resources: object schemas, effective customizations, report definitions. Prompts: month-end close, new-customer onboarding.
2. **Built-in assistant (optional UI).** Tenant configures any OpenAI-compatible/Anthropic/local endpoint. The assistant is just an MCP client with a chat UI — same tools, zero special access.
3. **Agent automations.** Scheduled or event-triggered agent runs (e.g., "every morning, draft collection emails for invoices >30d overdue") defined as automation actions with an agent budget.
4. **Ambient intelligence (each optional, each degradable):** semantic search everywhere (pgvector), duplicate detection (vendors/customers/invoices), anomaly flags on journal entries (amount/account/counterparty outliers), autocomplete for account coding learned from tenant history.

## Governance model (the important part)

- **Principal:** every agent session runs as a `service_principal` with a role. No role, no access. RLS applies identically.
- **Approval gates:** actions flagged `requires_human_approval` in metadata (defaults: anything that posts to GL, moves money, sends external communications, touches payroll) create an `approval_request` instead of executing. Approvals surface in-app + via notification; approval executes the held command atomically.
- **Budgets:** per-session and per-day caps on tool calls, rows read, and gated-action requests. Exceeding = hard stop + notify.
- **Audit:** `agent_audit` records every tool call: session, principal, tool, args (PII-redacted render + full encrypted payload), result summary, latency, approving human. Queryable; exportable; feeds the anomaly detector.
- **Kill switch:** tenant-level "pause all agents" toggle; per-principal revocation is instant (sessions check a revocation epoch).

## Design rules for module authors

1. Every capability must be reachable via API/MCP — UI-only features are bugs.
2. Tool descriptions are part of the product: written for an agent audience, with invariants stated ("posting requires an open period").
3. Destructive/gated tools must be idempotent and preview-able (`dry_run: true` support mandatory).
4. Embeddings/AI features must fail soft: no model configured → feature hides, workflow still works.

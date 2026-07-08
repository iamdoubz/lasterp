# ADR-008: AI-first — MCP-native, agents as governed principals

**Status:** Accepted · 2026-07-06

## Context
"AI-first, not an afterthought." By late 2026 every serious enterprise RFP asks for MCP; incumbents are bolting agents on top of closed cores. Being MCP-native from the schema up is a structural advantage no incumbent can retrofit cheaply.

## Decision
1. **Built-in MCP server** in the kernel. The metadata engine auto-generates MCP tools/resources for every object (core or custom): search, read, create-draft, transition-workflow, post (where permitted). Modules add task-level tools (`reconcile_bank_statement`, `generate_dunning_letters`, `close_period`).
2. **Agents are principals.** An agent session runs as a service principal with: a role (same RBAC as humans), per-session scopes, spend/action budgets, and **approval gates** — actions marked `requires_human_approval` (posting journals, paying vendors, sending payroll) enqueue for a human, never auto-execute unless a tenant explicitly loosens policy.
3. **Dedicated agent audit trail:** every tool call logs prompt context hash, tool, args, result, and the approving human (if any). Fully queryable; feeds anomaly detection.
4. **Semantic layer:** pgvector embeddings maintained by a kernel job for all text-bearing objects → semantic search API + MCP resource. Powers "find invoices like this", duplicate-vendor detection, natural-language reporting.
5. **BYO model:** LastERP never requires a specific LLM vendor. It exposes MCP (works with Claude, local models, anything); optional built-in assistant UI calls a tenant-configured model endpoint (incl. self-hosted). Zero AI features degrade the non-AI workflow.
6. **AI-legible design rule:** every workflow must be executable through the API/MCP surface alone — if a human can only do it by clicking, that's a bug.

## Rejected
- Proprietary assistant API instead of MCP: fights the standard.
- Autonomous-by-default agents: an ERP moves money; human-in-the-loop is the default posture.
- Mandatory hosted LLM: breaks self-hosting and data-sovereignty promises.

## Consequences
- MCP tool schemas are generated with the same codegen as REST — one source of truth.
- Approval-gate workflow engine is a kernel service (also used by non-AI approvals like PO thresholds).
- Embedding jobs must be cost-bounded and optional (self-hosters without a model still get FTS).

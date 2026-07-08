# 05 — Plugin System

Decision record: [ADR-007](adr/ADR-007-plugin-system.md). Extension surfaces, from least to most powerful:

| Surface | Skill needed | Mechanism |
|---|---|---|
| Custom fields, layouts, naming, saved views | Admin, no code | Tenant metadata overlay (ADR-006) |
| Automations: trigger → condition → action | Admin, no code | Workflow metadata; actions incl. email, webhook, field update, approval request |
| Declarative validations & computed fields | Power user | Expression language (CEL) in overlays |
| Server plugins: hooks, jobs, endpoints, MCP tools, tax/payroll providers | Developer, any language | WASM (Extism) |
| UI plugins: widgets, panels, pages | Frontend dev | ES modules in named slots / sandboxed iframes |
| Connectors | Developer | Declarative manifest + WASM transforms (07-INTEGRATIONS.md) |

## Plugin manifest (server plugin)

```yaml
id: com.acme.commission-calc
version: 1.2.0
lasterp: ">=1.0 <2.0"
capabilities:                    # admin approves each at install
  objects: [{type: Invoice, access: read}, {type: CommissionEntry, access: write}]
  http: [{host: api.acme.com, methods: [GET, POST]}]
  secrets: [acme_api_key]
  schedule: ["0 2 * * *"]
hooks:
  - {event: "invoice.posted", fn: on_invoice_posted, mode: async}
  - {event: "invoice.before_post", fn: validate_commission, mode: sync, timeout_ms: 500}
overlays: [./overlays/commission_entry.object.yaml]
mcp_tools: [{name: explain_commission, fn: explain_commission}]
endpoints: [{path: /report, fn: http_report, methods: [GET]}]
```

## Host runtime rules

- wazero runtime, per-invocation instance or pooled instances with reset; memory cap (default 64MB), wall-clock timeout (sync hooks 500ms default, async 30s, jobs 10m), CPU fuel metering.
- Host functions exposed to plugins: `object.query/get/create/update/transition` (capability-checked, RLS-scoped, audited), `http.request` (allowlist), `secrets.get`, `kv.get/set` (plugin-scoped storage), `log`, `emit_event`, `enqueue_job`.
- Sync hooks (`before_*`) may veto with a structured error; they run inside the request path so their latency budget is enforced ruthlessly. Async hooks get at-least-once delivery from JetStream with dead-letter + admin visibility.
- Failure isolation: plugin crash/timeout never corrupts a transaction; repeated failures trip a circuit breaker and notify admins.
- Versioned ABI (`lasterp-pdk/v1`); host guarantees compatibility within major version.

## Developer experience (make the afternoon-plugin promise true)

- `lasterp dev` — hot-reloading local instance with seed data.
- `lasterp plugin new --lang rust|go|ts|python` — scaffold with typed bindings generated from the tenant's effective schemas.
- `lasterp plugin test` — runs plugin against a golden in-memory instance; fixture recorder.
- Registry: signed bundles, semver, dependency solver, staged rollout (install to sandbox tenant first).

## UI plugin slots (v1 set)
`dashboard.widget`, `record.sidebar(object)`, `record.tab(object)`, `list.action(object)`, `nav.page`, `report.block`. Trusted (first-party/certified) plugins load as ES modules; untrusted load in sandboxed iframes with a typed postMessage bridge (read-only data access + command proposals only).

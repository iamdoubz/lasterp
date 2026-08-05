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

- wazero runtime, a fresh instance per invocation (nothing survives between calls, so one tenant's call leaves nothing behind for the next); memory cap (default 64MB = 1024 pages), wall-clock timeout (sync hooks 500ms default, async 30s, jobs 10m).
- **There is no CPU fuel meter, and this line used to promise one.** wazero has no fuel/gas counter — that is a wasmtime feature — and its interruption mechanism is context cancellation, which is wall-clock. The shipped controls are therefore: the **memory cap** (a memory bomb traps inside its own runtime; the host never allocates on its behalf), the **wall-clock deadline** (an infinite loop is closed out from under the module), and a **host-call budget** (default 1000 per invocation), which is the cheap stand-in for the "this plugin is spinning through the API" case fuel would otherwise catch. Corrected in WP-3.1a rather than left aspirational — see [WP-3.1-decisions.md](notes/WP-3.1-decisions.md) §2.
- Host functions exposed to plugins: `object.query/get/create/update/transition` (capability-checked, RLS-scoped, audited), `http.request` (allowlist), `secrets.get`, `kv.get/set` (plugin-scoped storage), `log`, `emit_event`, `enqueue_job`.
  - *Shipped in WP-3.1a:* `lasterp_log`, `lasterp_object_get`, `lasterp_object_query`, `lasterp_object_create`, `lasterp_object_update`, `lasterp_secret_get` — one JSON-in/JSON-out shape each. **The table is built from the plugin's approved capabilities**, which makes INV-X1 structural rather than defensive: an ungranted function is not refused at call time, it is absent from the module's imports and the plugin cannot be instantiated at all. Every call additionally runs through `authz.Authorize` under the plugin's own principal, so the manifest is the ceiling and authz is the gate.
  - *Deliberately absent until an audited client exists:* `http.request`. ADR-007 requires every outbound call be allowlisted **and audited**; the runtime's built-in client does the first and not the second, so the sandbox has no network at all and a manifest declaring `http:` is refused at install rather than partly honoured.
  - *Not yet built:* `emit_event`, `enqueue_job`, `kv.get/set`, `object.transition` — they arrive with the hook and job surfaces in WP-3.1b.
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

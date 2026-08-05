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
  - *Arriving with the hook surface (WP-3.1b):* `kv.get/set` — plugin- and tenant-scoped storage, which a real hook needs for a cursor or a dedupe key.
  - *Deferred out of WP-3.1b during its plan review, with owners rather than silence:* **`emit_event`** — letting untrusted code write the event store is INV-E territory and deserves its own design, not a line in a hook WP; **`enqueue_job`** and the manifest's **`schedule:`** capability — both need a job runner nothing owns yet, so they land with **WP-3.3 automations**, where scheduled triggers arrive anyway; **`object.transition`** — waits for a document whose lifecycle a plugin should be able to drive. Each was cut for want of a consumer, not for difficulty: building a host function with no caller is how an ABI acquires surface nobody tests.
- Sync hooks (`before_*`) may veto with a structured error; they run inside the request path so their latency budget is enforced ruthlessly. **They run *before* the database transaction opens, not inside it** (WP-3.1b): a plugin holding a Postgres transaction — or SQLite's single write lock — open for the length of its wall-clock budget is a throughput failure the sync engine already fought once. That ordering is also what makes INV-X2 structural rather than a runtime check: a plugin cannot partially commit a transaction because no plugin code ever runs inside one. The residual is stated rather than hidden — a hook that vetoes on state it read can be raced by a concurrent write before the commit, the same window ordinary validation has.
- **Sync-hook latency is the tenant's dial, not a hole in the p95 promise.** The default budget is 50ms; a manifest may raise it up to a hard ceiling it cannot exceed; and raising it shows the installing administrator, in plain language at approval time, what it costs per write of that object — the person who feels the latency is usually not the person who installed the plugin. Per-plugin hook latency is measured and attributed at runtime, because without that a slow plugin makes "the ERP is slow", which is the incumbent failure [docs/14](14-COMPETITIVE-PAIN-POINTS.md) exists to avoid. The docs/09 p95 budget describes the system LastERP ships; a tenant who adds work to every write has made a business trade-off, and the product's job is to price it, not to refuse it.
- **A plugin does not react to its own writes.** An `after_commit` write lands in the change feed, which would otherwise re-trigger the same plugin's hook; dispatch is suppressed for changes whose actor is that plugin (`plugin:<id>` is already the audit actor). Self-suppression rather than a depth counter, deliberately: the loop is cut at the source instead of bounded after the fact.
- Async hooks get at-least-once delivery with dead-letter + admin visibility. **The transport is the change feed, not a broker** (WP-3.1b): `changefeed.Read` already provides ordered, resumable, exactly-once-observed delivery with a cursor — that is INV-S5, proven since WP-2.1 — so an `after_commit` runner is a feed consumer with a per-plugin cursor, and solo mode stays one binary with no JetStream to deploy.
- Failure isolation: plugin crash/timeout never corrupts a transaction; repeated failures trip a circuit breaker and notify admins.
- Versioned ABI (`lasterp-pdk/v1`); host guarantees compatibility within major version.

## Developer experience (make the afternoon-plugin promise true)

- `lasterp dev` — hot-reloading local instance with seed data.
- `lasterp plugin new --lang rust|go|ts|python` — scaffold with typed bindings generated from the tenant's effective schemas.
- `lasterp plugin test` — runs plugin against a golden in-memory instance; fixture recorder.
- Registry: signed bundles, semver, dependency solver, staged rollout (install to sandbox tenant first).

## UI plugin slots (v1 set)
`dashboard.widget`, `record.sidebar(object)`, `record.tab(object)`, `list.action(object)`, `nav.page`, `report.block`. Trusted (first-party/certified) plugins load as ES modules; untrusted load in sandboxed iframes with a typed postMessage bridge (read-only data access + command proposals only).

# 15 — API & Developer Platform

Where docs/07 covers LastERP calling other systems, this doc covers **other systems (and developers) calling LastERP**. Foundation decisions: [ADR-008](adr/ADR-008-ai-first.md) (MCP), [ADR-009](adr/ADR-009-api-strategy.md) (REST/gRPC/webhooks).

## 1. The REST API (OpenAPI-compliant, dev-tool-native)

- **OpenAPI 3.1** (full JSON Schema alignment — the dominant standard) is generated, not hand-written, from object metadata + hand-extended endpoints. The spec is the contract; drift is impossible by construction.
- **Two spec flavors:**
  - *Core spec* — the shipped API, published in-repo and on the docs site; versioned with releases.
  - *Live tenant spec* at `GET /api/v1/openapi.json` — includes that tenant's custom objects, custom fields, and plugin endpoints. Your customizations are first-class API citizens, documented automatically.
- **Dev-tool interop:** because the spec is standard OpenAPI 3.1, it imports directly into Postman, Insomnia, Bruno, Hoppscotch, curl generators, and any iPaaS. We additionally publish:
  - An auto-generated **Postman collection** with a "Run in Postman" button (core API), and per-tenant collection download from the developer portal (includes custom objects, pre-configured auth + environment variables for base URL/token).
  - Request examples in the spec (`examples` on every operation) so imported collections are runnable, not skeletal.
- **Conventions (enforced by the generator, linted by Spectral in CI):** predictable resource paths (`/api/v1/{object}` + `:actions`), cursor pagination, `filter`/`sort`/`expand`/`fields` query grammar, RFC 7807 problem+json errors, idempotency keys on writes, `RateLimit-*` headers, ETags for optimistic concurrency on CRUD objects, ISO-8601 UTC everywhere.
- **Versioning promise:** `/v1` is stable for its lifetime; additive changes only; breaking changes = `/v2` with ≥12-month overlap; deprecations announced via `Deprecation`/`Sunset` headers and changelog.

## 2. Third-party authentication & authorization

| Consumer | Mechanism |
|---|---|
| Server-to-server integrations | **OAuth 2.1 client credentials** — tenant admin registers the app, grants a role + scopes |
| Apps acting for a user | **OAuth 2.1 authorization code + PKCE** — user consents; token carries user identity + app scopes (intersection applies) |
| Scripts, CI, personal tooling | **Personal Access Tokens** — scoped, expiring, revocable |
| Sync clients / MCP clients | Device-bound tokens / OAuth per §4 |

Scopes map onto the RBAC permission model (`invoicing:read`, `ledger:post`, `webhooks:manage`, per-custom-object scopes auto-generated) — one authorization brain for UI, API, sync, plugins, and AI (docs/08). Every token's calls are audited with the app identity distinct from the user identity.

## 3. Developer portal (ships with the product, self-hostable like everything else)

`developers.<instance>` serves: rendered reference docs from the live spec with **try-it console** (bound to a sandbox), getting-started guides + recipes (create invoice → record payment → reconcile), webhook event catalog with signature-verification samples, app registration & token management, per-app usage/rate-limit dashboards, changelog + deprecation tracker, and the Postman/SDK artifacts.

**Sandbox tenants:** any developer can spin up a disposable seeded sandbox (same shadow-tenant machinery as docs/13 WP-6.3) — realistic data, no risk. Hosted service offers free sandboxes; self-hosters get `lasterp sandbox create`.

## 4. The MCP server (agent-facing API, same governance)

Already core (docs/06); pinned implementation details to current spec norms:

- **Transports:** stdio (local/desktop) and **Streamable HTTP** (single endpoint, POST JSON-RPC + optional SSE stream) for remote/multi-tenant — the converged industry default; stateless-capable so it scales behind ordinary load balancers with the rest of the API tier.
- **Auth: OAuth 2.1 resource server** per the MCP authorization spec: PKCE mandatory, RFC 9728 protected-resource metadata for discovery, RFC 8414 AS metadata, **RFC 8707 resource indicators** so tokens are audience-bound to this MCP server. Dynamic client registration supported so any MCP client can onboard without manual setup.
- **No token passthrough:** the MCP server never forwards client tokens to downstream services (confused-deputy prevention); connector calls use their own vaulted credentials.
- Tool catalog is scope-filtered per caller: an agent authorized `invoicing:read` literally does not see posting tools. Tool list responses carry cache TTLs per spec so clients stay fresh across customization changes.
- Spec-version tracking is a standing maintenance task (MCP evolves fast; e.g. the 2026-07-28 revision's stateless core and Tasks extension for long-running work — the latter maps naturally onto our approval-gated commands).

## 5. SDKs

Generated from the core spec via OpenAPI Generator in CI (never hand-maintained): **TypeScript, Python, Go, C#** at launch. Idiomatic wrappers add: auth flows, pagination iterators, idempotency-key management, webhook signature verification, and typed custom-field access (generated per-tenant on demand from the live spec — `lasterp sdk generate`).

## 6. Webhooks (outbound, for completeness — detail in docs/07)
Every domain event subscribable; HMAC-SHA256 signatures with rotation, at-least-once + retries/backoff, dead-letter visibility, event catalog in the portal, thin payloads with fetch-back links (avoids leaking data through logs).

## Build plan
- **WP-3.7 Developer platform v1:** portal (reference + try-it + app registration), OAuth 2.1 flows + PAT + scopes, live tenant spec endpoint, Spectral lint gate in CI. AC: third-party demo app completes OAuth and full invoice lifecycle using only the portal; spec passes lint + round-trips through Postman and OpenAPI Generator cleanly.
- **WP-3.8 Postman & SDK pipeline:** collection generation + Run-in-Postman, four SDKs generated + smoke-tested in CI, per-tenant SDK/collection download. AC: `quickstart in <15 min` tutorial verified in CI using the published artifacts.
- **WP-3.4 (amended):** MCP server AC now includes Streamable HTTP transport + OAuth 2.1 resource-server conformance (PKCE, RFC 9728/8414/8707) + no-token-passthrough test.
- **WP-4.11 Sandbox self-service** (depends on shadow-tenant machinery). AC: developer sandbox from zero in <2 min.

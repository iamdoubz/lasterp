# ADR-009: API & eventing — REST public, gRPC internal, NATS JetStream async

**Status:** Accepted · 2026-07-06

## Decision
- **Public API: REST/JSON**, OpenAPI-generated from object metadata, versioned (`/api/v1/`), cursor pagination, idempotency keys on all writes (`command_id`), consistent RFC-7807 problem responses.
- **Internal + sync protocol: gRPC** (streams for sync; efficiency for high-frequency paths). gRPC-web for browser sync channel; falls back to WebSocket.
- **Outbound events: webhooks** (HMAC-signed, retried with backoff, dead-letter visible in admin UI) + **NATS JetStream** subjects for in-cluster consumers (projectors, plugin async hooks, connectors).
- **GraphQL: not in v1.** The metadata-driven REST + rich query params (filter/sort/expand) covers the need; revisit on demand.
- Single gateway enforces: authn, tenant context, rate limits (per token + per tenant), request logging.

## Rationale
- Idempotency keys + command IDs unify online writes, offline replay (ADR-004), and integration retries — one exactly-once story everywhere.
- NATS embeds in the Go binary → solo mode needs no external broker; cluster mode runs NATS clustered.

## Consequences
- Every module's API is generated + hand-extended, never hand-built from scratch.
- Public API compatibility is a release gate: contract tests run against the previous minor version's recorded interactions.

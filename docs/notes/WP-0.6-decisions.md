# WP-0.6 — API gateway: interpretation & decisions

WP-0.6 (docs/11): "API gateway: REST routing from metadata, OpenAPI
generation, idempotency keys, problem+json errors, rate limiting. AC:
OpenAPI spec validates; idempotent replay returns identical result."

Foundation: ADR-009 (REST public, OpenAPI-generated, idempotency keys,
RFC-7807), docs/15 (OpenAPI 3.1, `GET /api/v1/openapi.json` live spec,
`RateLimit-*` headers, cursor pagination convention). Builds on the WP-0.5
metadata engine (`kernel/metadata`: `EffectiveSchema` + `CRUD`).

## Decisions

1. **stdlib routing, no router dependency.** `net/http.ServeMux` (Go 1.22
   method+pattern routing, already used by `kernel/api`) covers everything.
   Routes are registered per registered object from its `EffectiveSchema`:
   - `GET    /api/v1/{name}`        → List   (returns `{"data":[...]}`)
   - `POST   /api/v1/{name}`        → Create (201)
   - `GET    /api/v1/{name}/{id}`   → Get
   - `PATCH  /api/v1/{name}/{id}`   → Update
   - `DELETE /api/v1/{name}/{id}`   → SoftDelete (204)
   `{name}` is `strings.ToLower(ObjectName)`. No pluralization logic (YAGNI).
   Unmatched routes fall through to a `/` handler returning problem+json 404.

2. **Idempotency: reserve-first, `idempotency_keys` table.** CLAUDE.md /
   ADR-009 require idempotency keys on *all* writes, so POST/PATCH/DELETE
   without an `Idempotency-Key` header are rejected 400. On a write we:
   (a) reserve a pending row `(tenant_id, idem_key, request_fingerprint,
   status=0)`; (b) if the key already exists → replay the stored response
   (status+body) when the fingerprint matches, 409 if it doesn't (same key,
   different request) or if still pending; (c) execute; (d) on 2xx finalize
   the row with the captured status+body, otherwise discard the reservation
   so a failed write can be retried. Fingerprint = SHA-256 of
   method+path+body. Table is `tenant_id`-first, PK `(tenant_id, idem_key)`,
   Postgres RLS enabled (SQLite no-op, per the existing RLS migrations).
   - *Known limitation (documented, not a bug):* reservation and the CRUD
     mutation run in separate transactions (the CRUD engine owns its own
     `WithTenant` tx and isn't refactored here). A crash between the CRUD
     commit and `finalize` leaves a pending row; the next replay returns 409
     in-progress rather than the result. Full single-tx atomicity is a
     larger engine change deferred to a later WP. This preserves *no double
     effect* (the point of idempotency); it does not yet guarantee *replay
     always returns the cached body* across a mid-flight crash.

3. **problem+json (RFC 7807).** `application/problem+json` with
   `type/title/status/detail/instance`. Domain errors map by `errors.Is`:
   `metadata.ErrValidation`→422, `metadata.ErrRecordNotFound`→404,
   `authz.ErrPermissionDenied`→403, `authz.ErrNoActor`→401, malformed JSON /
   missing idempotency key→400, idempotency conflict→409, rate limit→429,
   anything else→500 (no internal detail leaked).

4. **Rate limiting: hand-rolled token bucket, no new dependency.**
   `golang.org/x/time/rate` would be the obvious pick but it isn't a current
   dependency and a new runtime dep needs an ADR (CLAUDE.md). A ~40-line
   token bucket keyed per `(tenant, actor)` (ADR-009: per token + per
   tenant) covers it. Emits `RateLimit-Limit/Remaining/Reset` and, on 429,
   `Retry-After`. Disabled when limit is zero; `NewGateway` defaults to a
   generous limit so normal use is never throttled.

5. **Authentication is an injected seam, not built here.** No HTTP auth WP
   has landed (OAuth 2.1 / PAT are WP-3.7, docs/15). The gateway takes an
   `Authenticator` that maps a request → `(authz.Actor, tenancy.ID)`; it is
   the choke point that binds the actor to the context (`authz.WithActor`)
   and supplies the tenant to the CRUD engine. A `nil` authenticator fails
   closed (401 on every CRUD route). Real token verification is deferred;
   the metadata CRUD engine already enforces INV-T2/T4 via `authz.Authorize`
   underneath, so this WP does not weaken any invariant.

6. **OpenAPI 3.1 generated from metadata.** `OpenAPI(objects...)` builds the
   spec (JSON Schema 2020-12 aligned) from each `EffectiveSchema`: a
   component schema per object (fields typed from `FieldType`), collection +
   item paths, the shared `Problem` response, the `Idempotency-Key`
   parameter on writes. Served live at `GET /api/v1/openapi.json`. The core
   spec (kernel registry — currently only the envelope, since the kernel
   ships no domain objects yet) is committed at `api/openapi.json` and kept
   current by a golden test (`go test ./kernel/api -run Golden -update`);
   module WPs that ship core objects regenerate it in their own PR.
   - Full Spectral/OpenAPI-validator linting is WP-3.7's CI gate (docs/15).
     Here "spec validates" is enforced by a structural test asserting the
     OpenAPI 3.1 required shape (openapi/info/paths/components, every
     operation has responses).

7. **Out of scope (noted, not built):** cursor pagination, `filter/sort/
   expand/fields` query grammar, ETags/optimistic concurrency, gRPC,
   webhooks, OAuth flows. All are later-WP concerns per docs/15; List
   returns the full non-archived set in a `{"data":[...]}` envelope that
   leaves room for a `next_cursor` sibling without a breaking change.

## Threat model / security notes

This WP sits on the authn/authz/tenancy request path, so per CLAUDE.md's DoD
here is the explicit threat model. The gateway is the single choke point
(ADR-009) and fails closed on every branch.

| Threat | Vector | Mitigation |
|---|---|---|
| **Spoofed / forged principal** | Request arrives with no or a bogus identity | The `guard` middleware requires an `Authenticator`; a `nil` authenticator or any `Authenticate` error returns 401 and never reaches the CRUD engine. Actor identity is never derived from request parameters — only from what the Authenticator returns (which, once real, is a verified token — WP-3.7). No anonymous writes reach `authz.Authorize` (INV-T4). |
| **Cross-tenant access** | Read/write another tenant's rows | Every CRUD call runs through `tenancy.WithTenant` (RLS backstop on Postgres, explicit `tenant_id` filter on SQLite — INV-T1) and `authz.Authorize` (INV-T2). The tenant the write lands in is taken from `actor.TenantID`, the *same* value authz filters on — see the next row. |
| **Tenant-consistency confusion** | A buggy/hostile `Authenticator` returns an `(actor, tenant)` pair whose `actor.TenantID` differs from the returned `tenant`, so authz checks tenant A but the write lands in tenant B | `guard` rejects `actor.TenantID != tenant` with a 403 before doing anything, and passes `actor.TenantID` (not the separately-returned value) to the CRUD engine, so the authorization tenant and the storage tenant are provably the same. Covered by `TestTenantMismatchRejected`. |
| **Replay / duplicate submission** | Network retry or malicious resend double-executes a write (double invoice, double payment) | Idempotency keys (ADR-009): reserve-first `idempotency_keys` row means a duplicate observes the reservation and replays the stored response instead of re-executing. Fingerprint mismatch on a reused key → 409, never a silent wrong replay. Proven by `TestIdempotentReplayReturnsIdenticalResult` (no double-insert). |
| **Rate-limit DoS** | A caller floods the gateway to exhaust CPU/DB | Per-`(tenant, actor)` token bucket returns 429 + `Retry-After` past the burst; `RateLimit-*` headers advertise the budget. Keyed after authentication so one tenant/token cannot spend another's budget. |
| **Idempotency-reservation spam** | A caller inserts many pending keys to bloat the table, or wedges a key by never completing | Reservations are tenant-scoped (RLS) and bounded by the same rate limiter as any other write. A write that fails (non-2xx) *discards* its reservation so it cannot wedge a key; a key stuck pending only blocks that one client's own key (409 in-progress), not others. Idle-row GC is future work (noted below), not a correctness gap. |

Residual/accepted (future work, not blockers): the rate-limit bucket map and
the idempotency table are unbounded in-memory / on-disk respectively (no idle
eviction / TTL GC yet); the limiter is per-process (not shared across a
horizontally-scaled gateway tier); the 401 body echoes the Authenticator's
error text. None widen an invariant; all are deferred refinements.

## Invariants

The invariant registry-as-code is WP-0.8 (not yet built), so tests are
tagged by name rather than registered. The gateway preserves INV-T1 (every
CRUD call goes through `tenancy.WithTenant`), INV-T2/INV-T4 (every write
goes through `authz.Authorize` inside the CRUD engine; the gateway binds an
attributable actor). Idempotent-replay-returns-identical-result (ADR-009,
the exactly-once spirit of INV-E4 applied to CRUD writes) is covered by
`TestIdempotentReplayReturnsIdenticalResult`.

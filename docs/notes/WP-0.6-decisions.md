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

## Invariants

The invariant registry-as-code is WP-0.8 (not yet built), so tests are
tagged by name rather than registered. The gateway preserves INV-T1 (every
CRUD call goes through `tenancy.WithTenant`), INV-T2/INV-T4 (every write
goes through `authz.Authorize` inside the CRUD engine; the gateway binds an
attributable actor). Idempotent-replay-returns-identical-result (ADR-009,
the exactly-once spirit of INV-E4 applied to CRUD writes) is covered by
`TestIdempotentReplayReturnsIdenticalResult`.

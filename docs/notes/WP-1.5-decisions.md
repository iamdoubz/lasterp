# WP-1.5 — Web client v1: interpretation & decisions

Roadmap entry ([docs/11-ROADMAP.md](../11-ROADMAP.md)): *"Web client v1: metadata-rendered
list/form/detail, auth, navigation, LastERP UI kit foundations. AC: Playwright e2e on invoice
lifecycle; p95 budget smoke."*

Design inputs read: [ADR-010](../adr/ADR-010-frontend.md) (frontend),
[ADR-006](../adr/ADR-006-metadata-customization.md) (metadata),
[ADR-009](../adr/ADR-009-api-strategy.md) (API), [docs/03](../03-DATA-MODEL.md),
[docs/09](../09-SCALABILITY-DEPLOYMENT.md) (budgets), [docs/17](../17-LOCALIZATION-ACCESSIBILITY.md)
(i18n/a11y), [docs/19](../19-DATA-INTEGRITY.md) (always).

The WP entry links no doc of its own and the ADR describes the *end state* of the client, not its
v1. Everything below is the interpretation this WP builds to.

---

## 1. Offline / SQLite-WASM local replica is NOT in this WP

ADR-010 says the client reads from a SQLite-WASM replica over OPFS, maintained by the sync client
(ADR-004). The sync engine is **Phase 2** (WP-2.x); WP-1.5 sits in Phase 1 and its ACs say nothing
about offline. Building a replica before the sync protocol exists would mean inventing the protocol
here — exactly the kind of side door CLAUDE.md forbids.

**Decision:** v1 reads and writes the REST API directly. Not a deviation from ADR-010, a phasing of
it — commandment 4 ("offline is the default") lands with the sync client.

**Consequence we accept now:** all server-state access goes through one module (`web/src/api/`), so
Phase 2 swaps the fetcher for a local-replica read without touching screens. We do **not** build an
abstraction layer for that swap today — one module boundary is the whole seam.

## 2. "Metadata-rendered" means rendered from the *field schema*, not from a UI descriptor

docs/03 lists "UI descriptors" among the artifacts the metadata engine generates, but
`kernel/metadata.Object` (schema.go) has no UI/layout/view section today — only
`Field{Name, Type, Required, Target, Index}` over the closed `FieldType` set. Inventing a UI
descriptor DSL here would be an ADR-006 change smuggled in through a client WP.

**Decision:** derive the UI from the existing closed type set — one `FieldType → control` map is the
entire "renderer". `text→input`, `long_text→textarea`, `money→minor-unit currency input`,
`date→<input type="date">`, `bool→checkbox`, `enum→select`, `link→reference picker`, and so on.
Required/optional and validation come from `Field.Required` + the server's problem+json.

Field labels resolve from i18n key `object.<Object>.field.<name>` and fall back to the field name,
so the hardcoded-string lint gate stays green without a translation file per module.

**Deferred (flagged):** column ordering, section grouping, per-view field visibility, and widget
overrides all want a real UI descriptor in the object schema. That is a metadata-engine change with
an overlay story (a tenant must be able to re-order fields without forking) — it belongs in its own
WP against ADR-006, not here. v1 renders fields in schema order.

## 3. The client needs a schema endpoint — `GET /api/v1/meta/objects`

Nothing today serves effective schemas over HTTP; the gateway serves data and OpenAPI only. A
metadata-rendered client cannot exist without it.

**Decision:** add a read-only, authenticated, capability-gated action returning the effective schema
for every registered object. Tenant-scoped like every other route (overlays are per-tenant, so this
is INV-T1 surface, not a static document). OpenAPI is *not* a substitute — it describes routes, not
the field-level metadata the renderer needs, and it is not overlay-aware per tenant.

## 4. Login is the one public route — and that is a new hole in the gateway

`internal/app/auth.go:25` explicitly defers HTTP login/session issuance to this WP. But every
gateway route today goes through `guard`, which 401s when there is no `Authenticator` result — a
login route cannot authenticate before it has issued a session.

**Decision:** add `Action.Public bool` to `kernel/api`. A public action skips **only** the authn step;
it still gets rate limiting (keyed by client IP instead of actor, since there is no actor yet),
problem+json errors, and OpenAPI documentation. Routes: `POST /api/v1/sessions` (login),
`POST /api/v1/sessions/refresh`, `DELETE /api/v1/sessions/current` (logout — authenticated, not
public).

**This is the security-sensitive part of the WP** and gets a threat-notes section in the PR. It is
also a standing INV-T2 risk: a future WP marking a *write* route `Public: true` silently removes the
"no write without an authenticated principal" guarantee. Mitigation is a tagged test, not a comment
— see §7.

### 4a. Login carries an explicit tenant

`idx_users_tenant_email` makes email unique *per tenant* — the same address can be a user in two
tenants by design — so there is nothing for the server to infer a tenant from. The login body takes
`tenant` explicitly and the sign-in form asks for it. A deployment fronting LastERP with per-tenant
subdomains can fill it in at the edge; that is a deployment concern, not a v1 one.

## 5. Token handling in the browser: httpOnly cookies, not JS-readable storage

`identity.IssueSession` already returns an opaque session token + refresh token. The question is only
where the browser keeps them.

**Decision:** the login response sets both as `HttpOnly; Secure; SameSite=Strict` cookies and returns
no token in the body. JS never touches a credential, so an XSS in a rendered field cannot exfiltrate
a session. The gateway's authenticator accepts the session cookie *in addition to* the existing
`Authorization: Bearer` header, so API/MCP clients and the boot e2e are unaffected.

CSRF is covered by `SameSite=Strict` plus the existing ADR-009 rule that every write requires an
`Idempotency-Key` header — a cross-origin form post cannot set a custom header, and a cross-origin
`fetch` that tries is stopped by preflight.

**Rejected:** access token in `sessionStorage` (one XSS = one stolen session, and the metadata
renderer displays tenant-authored strings), and a body-returned token the SPA holds in memory (loses
the session on every reload, so it needs the refresh cookie anyway — same plumbing, strictly worse).

## 6. UI kit foundations = the components these three screens actually need

ADR-010 names "Tailwind + shadcn-style component library (LastERP UI Kit)". shadcn-style means
copied-in components, not a dependency, so there is no new runtime dep to justify. Tailwind and
TanStack are already named by the ADR and need no new ADR.

**Decision:** build only what list/form/detail/login consume — `Button`, `Input`, `Select`,
`Checkbox`, `Field` (label + error + description), `Table`, `Toast`. Each ships keyboard-operable,
with visible focus, correct ARIA, and logical CSS properties only (docs/17: RTL from the first
component). No component exists that no screen renders.

**TanStack Router + Query: yes.** Routing and server-state caching are load-bearing for navigation
and the p95 read budget.

**TanStack Table: deferred, and this is a conscious deviation from ADR-010's letter.** v1 lists are
a semantic `<table>` with no column config, sorting, or virtualization to drive; the dep earns its
place when reports need column selection (WP-1.6). ADR-010's intent (virtualized, keyboard-first
lists) is not contradicted — it is unmet until there are lists long enough to virtualize. Flagged
here rather than silently skipped.

## 7. Invariants

No new INV-* catalog entry. This WP adds no new invariant-bearing storage or financial code — it
exposes existing pipelines over HTTP and renders them. Invariants it *touches*, with the tagged tests
that hold them:

| Invariant | How WP-1.5 touches it | Test |
|---|---|---|
| **INV-T1** | `/meta/objects` is a new tenant-scoped read path; overlays differ per tenant | tagged test: two tenants, divergent overlays, neither sees the other's schema |
| **INV-T2** | `Action.Public` is a new authn bypass | tagged test: enumerate every registered route, assert the public set is exactly the login/refresh allowlist and that **no** public route has `Write: true`; every other route 401s unauthenticated |
| **INV-T4** | login/logout are attributable mutations | tagged test: session issue/revoke land in `audit_log` with actor + timestamp |
| INV-F2/F3/F5/F6 | exercised end-to-end by the Playwright invoice flow (draft → post → GL → PDF) | not re-enforced here; the e2e is a witness, the module tests remain the proof |

The INV-T2 route-enumeration test is the important one: it turns "don't mark writes public" from a
review convention into a CI failure.

## 8. p95 budget smoke: a Go harness, not k6

docs/09 specifies "k6 + custom harness" for the budget table (interactive read p95 < 100ms, write
p95 < 300ms).

**Decision:** v1 measures the server budget with a Go test (`//go:build perf`) driving the
boot-assembled handler over `httptest`, on SQLite in CI. It reuses the existing boot harness, adds no
CI tooling, and fails the build on regression — the point of the gate. k6 arrives with the load-test
job when there is a deployed target to point it at (docs/09's nightly full-scale test), which does
not exist in Phase 1.

The *client* budget (< 30ms from local replica) is not measurable until Phase 2 — see §1.

## 9. Accessibility gate

docs/17 line 28: "axe-core in CI from WP-1.5 onward". **Decision:** every Playwright flow runs an
axe-core scan; any serious/critical violation fails the job. This is a new CI job (`e2e`), not an
extension of an existing one, because it needs a browser and a running server.

## 10. Bootstrapping: `lasterp bootstrap` (scope the AC forced open)

The AC says "Playwright e2e on invoice lifecycle" and "auth". Neither is possible on a freshly
migrated database, because **nothing could create the first tenant or user**. Tenant provisioning is
deliberately not an API (self-service tenant creation is not a thing LastERP does), so there was no
way in at all.

**Decision:** add `lasterp bootstrap --tenant --name --email` (password from
`LASTERP_BOOTSTRAP_PASSWORD`, so it stays out of shell history and the process table). It creates the
tenant, an administrator with an explicit grant list, and enables the built-in modules.

The grant list is written out rather than granted by wildcard: authz has no wildcard, and a bootstrap
that granted "whatever objects exist" would silently widen itself every time a module was added.

## 11. Bugs this WP surfaced and fixed

Driving the real product end-to-end found four defects that no existing test could have caught,
because every prior harness booted a fresh database and called Go functions directly.

1. **`lasterp serve` could not restart.** `Setup` re-registers the built-in modules on every boot, and
   `SaveObjectSchema` was an unconditional INSERT — so the second start against an existing database
   died on a UNIQUE violation. Every test fixture booted a *fresh* database, so nothing caught it.
   Fixed by making an identical re-save a no-op and a *changed* definition at the same version
   `ErrSchemaVersionConflict` (never a silent overwrite — that would rewrite history for every tenant
   on that version). Covered by `restart_integrity_test.go`.
2. **Unclassified 500s were logged nowhere.** `fail()`'s default branch wrote an empty problem+json
   and dropped the error entirely — undebuggable in production and in CI. It now logs server-side
   while still keeping internals out of the response body.
3. **`ledger.ErrAccountNotFound` mapped to 500.** A bad account id in a request body is a 422.
4. **`capability.ErrModuleInUse` mapped to 500.** Refusing to disable a module something depends on is
   a legitimate, explainable 409 (ADR-018 closure) — the caller needs to know which modules block it.

A WCAG 2.2 target-size failure (`<Link>` wrapping `<Button>` collapses the anchor to 2px) was caught
by the axe gate on its first run, which is the gate earning its place on day one.

---

## Deferred, flagged for later WPs

- Offline replica + sync client → Phase 2 (§1)
- UI descriptors in the object schema: field order, grouping, widget overrides → own WP vs. ADR-006 (§2)
- TanStack Table / virtualization → WP-1.6 when column config is real (§6)
- OIDC login → WP-1.9 (already scheduled); the login route is built so a second provider slots in
- TOTP challenge in the login flow: `identity.ValidateTOTP` exists and the route handles it, but the
  enrollment UI (QR, recovery codes) is not in v1
- Dashboards, reconciliation workbench, and other hand-built screens → WP-1.8 / Phase 4

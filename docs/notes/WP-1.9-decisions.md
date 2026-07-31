# WP-1.9 decisions — OIDC AuthProvider

**Status: in progress (2026-07-31).** Branch `wp-1.9`.

WP-1.9 is the last WP in Phase 1. It fills the hole WP-0.3 left deliberately:
`kernel/identity.AuthProvider` has had exactly one implementation
(password+TOTP) since Phase 0, with OIDC deferred pending a JOSE ADR
(docs/notes/WP-0.3-decisions.md §1). That ADR is
[ADR-019](../adr/ADR-019-jose.md), written as part of this WP.

## Invariants touched

No new catalog entries, and no `TestRequired` flips — all three of these were
already enforced and tested. This WP adds new paths that must not break them.

| Invariant | Why it applies here |
|---|---|
| **INV-T1** | The `(issuer, subject)` → user lookup is tenant-scoped. A subject linked in one tenant must not authenticate into another, on either dialect. |
| **INV-T2** | Two new `Public` routes — two new authentication bypasses. `internal/app/routes_integrity_test.go` is the fence and its allowlist grows by exactly these two. |
| **INV-T4** | An OIDC login is a mutation (it issues a session) and must be attributable: an `audit_log` row with actor, session and action, same as the password path. |

## Ambiguities resolved

**1. JOSE library (the WP's own deliverable).** Stdlib verifier, no new
dependency. Full reasoning in [ADR-019](../adr/ADR-019-jose.md); the short
version is that the useful part is small, the unused part of a JOSE library is
where the CVEs live, and the repo has consistently taken this trade before
(TOTP, migration runner, PDF, XLSX, ECB XML).

**2. Provider configuration lives in deployment environment variables**, not a
per-tenant table: `LASTERP_OIDC_ISSUER`, `LASTERP_OIDC_CLIENT_ID`,
`LASTERP_OIDC_CLIENT_SECRET`, `LASTERP_OIDC_TENANT`, `LASTERP_OIDC_REDIRECT_URL`.
One IdP per deployment, matching `LASTERP_DSN` / `LASTERP_ADDR` /
`LASTERP_BOOTSTRAP_PASSWORD`.

The alternative — an `oidc_providers` table so each tenant configures its own
IdP over the API — is the right long-term shape for multi-tenant SaaS, but it
requires storing an OAuth client secret at rest. docs/08 specifies a secrets
vault for exactly this and nothing has built one yet, so the honest options
today were "plaintext client secret in the database" (a security regression this
WP would be introducing, not inheriting) or "build envelope encryption in an
auth WP" (a second WP wearing a trenchcoat). Env config defers the question
without pretending to answer it. **Per-tenant IdP configuration is blocked on a
secrets vault, not on this decision.**

Consequence worth naming: because the tenant comes from configuration, the OIDC
login routes do not accept a tenant from the request at all. That removes a
whole class of confused-deputy bug the password path has to handle explicitly
(`loginReq.Tenant`), and it is why the state cookie carries no tenant.

**3. No just-in-time user provisioning.** Resolution order on callback:

1. `users` row matching `(tenant_id, oidc_issuer, oidc_subject)` → that user.
2. Otherwise, *only if* the ID token asserts `email_verified: true`, a `users`
   row matching `(tenant_id, email)` → link it (persist issuer+subject) and
   return that user. Subsequent logins take path 1.
3. Otherwise `ErrInvalidCredentials` — the same undifferentiated error the
   password path returns, so the callback cannot be used to enumerate which
   emails exist in the tenant.

The IdP therefore cannot create principals in a LastERP tenant; an administrator
(or `lasterp bootstrap`) creates the user, and the first SSO login binds it.
JIT provisioning would not have saved a step anyway: a JIT-created user holds no
role grants, so an admin action is required before they can do anything.

The `email_verified` requirement on step 2 is the account-takeover guard — an
IdP that lets a user set an arbitrary unverified email would otherwise be able
to claim any local account by email. Once linked, matching is by subject, so a
later email change at the IdP neither breaks nor hijacks the link.

**4. Storage: two columns on `users`, not a `user_identities` table.**
`oidc_issuer` + `oidc_subject`, with a unique index
`(tenant_id, oidc_issuer, oidc_subject)` — tenant first, per CLAUDE.md. NULLs
compare as distinct in unique indexes on both Postgres and SQLite, so the many
password-only users coexist without a partial index (which the two dialects
spell differently). A join table would only buy multiple IdPs per user, which
nothing asks for; one IdP per deployment (§2) makes it unreachable by
construction today.

**5. The state cookie is `SameSite=Lax`, not `Strict`.** Every other cookie the
product sets is `Strict` (WP-1.5-decisions.md §5) and that is correct for them.
It is *wrong* here and would break login completely: a `Strict` cookie is not
sent on a top-level cross-site navigation, which is exactly what the IdP's
redirect back to the callback is. The browser would arrive at the callback with
no state cookie and the request would fail closed, every time.

`Lax` is safe for this cookie because it is not a credential: it holds the
CSRF `state`, the `nonce`, and the PKCE code verifier, all single-use, all
minted seconds earlier, and the cookie is cleared at the callback. The
protection it provides (an attacker cannot complete a login flow they did not
start) survives `Lax` because `Lax` still withholds the cookie from cross-site
*subresource* requests and non-idempotent cross-site posts.

**6. Two public routes, both reads.**

- `GET /api/v1/sessions/oidc` — mints state/nonce/PKCE, sets the state cookie,
  returns the authorization URL. Not a redirect, because the web client needs to
  know whether SSO is configured at all in order to decide whether to render the
  button; a 404 here is that answer.
- `GET /api/v1/sessions/oidc/callback` — the IdP's redirect target. It is a
  `GET` that has an effect (it issues a session), which is not RESTful, and is
  also not optional: the OIDC redirect is a browser navigation. It is registered
  `Write: false` because `Action.Write` means "requires an `Idempotency-Key`
  header", and no IdP will send one.

**Both routes are only registered when OIDC is configured.** An unconfigured
deployment does not carry a dead public route, and the web client gets a clean
404 to branch on instead of a config endpoint that would itself have to be
public.

**7. The `Credentials` struct grows three fields** (`Code`, `CodeVerifier`,
`Nonce`) rather than the interface changing. `Credentials` already documents
itself as provider-specific — "PasswordTOTPProvider reads Email, Password, and
TOTPCode" — so a second provider reading three different fields is the shape
that was designed for, and satisfies the WP's "behind the existing interface"
literally.

**8. The Keycloak e2e is a Go integrity test, not a Playwright test.** It drives
the real `internal/app` HTTP surface through the entire flow against a real
Keycloak container (testcontainers, already a dependency, generic container API
so no new module): start → IdP login form → redirect with code → callback →
session cookie → authenticated call to a real API route. It carries
`//go:build integrity` so the gauntlet picks it up with no `ci.yml` change
(WP-0.10 convention), and skips under `-short` like every other container test.

Putting Keycloak into the Playwright stack instead would add a container to the
browser e2e for a flow that has no interesting client-side behaviour — the
browser's entire contribution is following two redirects.

## Known rough edges (accepted for v1, named so they are not discovered)

- **A failed callback renders problem+json, not a page.** The callback answers
  `401 application/problem+json` on every refusal, which is the product's
  uniform error contract but is a raw JSON blob in a browser — the visible
  result of declining consent at the IdP. Redirecting to the sign-in screen with
  a flag would read better and was rejected for v1 because dressing a 401 as a
  302 is its own smell and the failure is rare. Worth revisiting the first time
  someone actually sees it.
- **Two sign-in tabs share one flow cookie.** Starting a second login overwrites
  the first tab's state, so completing the *first* tab afterwards fails closed
  with a 401 rather than succeeding. Correct, but confusing. Fixing it means
  keying the cookie per attempt or moving pending flows server-side; neither is
  worth it before anyone hits it.

## Deferred (flagged, not forgotten)

- **Per-tenant IdP configuration** — blocked on a secrets vault (§2).
- **Multiple IdPs per tenant / per user** — unreachable while §2 holds.
- **SAML, SCIM provisioning** — docs/08 enterprise tier, no WP yet.
- **Back-channel logout, IdP-initiated logout, refresh against the IdP.** LastERP
  sessions are independent of IdP session lifetime once issued: revoking access
  at the IdP does not revoke a live LastERP session before its TTL. Documented
  limitation, not a bug, and the same trade every opaque-session product makes.
- **Front-channel flows (implicit/hybrid).** Deliberately never implemented; see
  ADR-019.
- **`prompt`/`max_age`/step-up authentication, ACR/AMR claims.** No consumer.
- **Group/role claim mapping to LastERP roles.** Authorization stays entirely
  local — the IdP authenticates, it does not authorize. Wiring IdP groups to
  role grants is an authz change and belongs with whatever WP does SCIM.

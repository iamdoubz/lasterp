# WP-0.3 decisions & blockers

**Status: unblocked, in progress (2026-07-08).**

WP-0.2 (storage adapters) merged (`kernel/storage`, `kernel/storage/migrate`,
Postgres/SQLite conformance suite). The prior blocker in this file — "no
storage code written under WP-0.3 until WP-0.2 lands" — no longer applies.
WP-0.3 is a foundational Phase-0 WP (not a module WP), so the WP-0.8
integrity-foundation gate doesn't apply either (docs/11-ROADMAP.md dependency
rule 1: "never start a *module* WP before WP-0.8").

## Ambiguities resolved

**1. OIDC scope.** The WP text says "OIDC + password/TOTP". A real OIDC
relying-party implementation (discovery, JWKS fetch/cache, ID-token
validation) needs either a JWT/OIDC library (`coreos/go-oidc`,
`golang-jwt/jwt`, ...) or a hand-rolled JOSE stack — both are more than a
"no new runtime dependency without an ADR" (CLAUDE.md) call I can make
unilaterally, and neither has test value without a real IdP to integrate
against. **Decision:** ship an `AuthProvider` interface in `kernel/identity`
with one concrete implementation now (password + TOTP). OIDC becomes a
second implementation of the same interface in a follow-up WP once an ADR
picks a JOSE library. This satisfies the AC's authentication surface
(principals authenticate, sessions issue) without inventing unreviewed
dependencies.

**2. RBAC condition language.** 08-SECURITY-MULTITENANCY.md specifies
permission conditions as "an optional CEL expression over record + actor".
`cel-go` is not currently a dependency, and WP-3.3 (Automations) already
plans to introduce CEL for workflow conditions — adding it twice (here and
in WP-3.3) risks two divergent evaluators. **Decision:** the `role_permission`
row stores a `condition` column (nullable text) so the schema is
forward-compatible, but WP-0.3's evaluator only supports the empty condition
(unconditional grant) — non-empty conditions are stored but rejected at
grant time with a clear "not yet supported" error rather than silently
ignored. Full CEL evaluation lands whenever WP-3.3 (or WP-0.5 metadata
engine, whichever lands first) brings in `cel-go` under its own ADR.

**3. Session token format.** No JWT library is present and none is needed:
per-tenant, per-device opaque bearer tokens (crypto/rand, 256 bits) work
with the storage-adapter pattern already built, are trivially revocable
(delete/mark-revoked a DB row — a JWT can't be revoked without a
denylist anyway), and match "refresh bound to device" (08-SECURITY-...md)
via a `device_id` column. Tokens are stored hashed (SHA-256) at rest, never
in plaintext, so a DB read doesn't leak usable credentials.

**4. TOTP.** RFC 6238 is ~40 lines over stdlib `crypto/hmac` + `crypto/sha1`
+ `encoding/binary` — no dependency needed.

**5. Password hashing.** `golang.org/x/crypto/bcrypt` is already an
*indirect* dependency (pulled in transitively today). Promoting it to a
direct import for password hashing is not "a new runtime dependency" in the
sense CLAUDE.md guards against (no new module enters `go.sum`); flagging it
here for visibility rather than opening an ADR for a golang.org/x package
already in the dependency graph.

**6. Tenant scoping of `users`.** A person with access to multiple tenants
gets one `users` row per tenant (row-level multi-tenancy per ADR-005 — every
table carries `tenant_id`), not one global identity fanned out via a join
table. Simplest model consistent with "every table carries tenant_id, shared
schema + RLS default."

**7. `sessions` is exempt from RLS.** Session lookup by bearer token happens
*before* tenant context exists — the token is what tells the middleware
which tenant to set, so an RLS policy keyed on `app.tenant_id` would make
every token lookup return zero rows, breaking login entirely. `sessions` is
treated as a system table (like `tenants`), not tenant-RLS'd. This is safe
because the row returned is uniquely determined by an unguessable 256-bit
token plus its own tenant/user/expiry/revoked columns — a leak here exposes
one session, not another tenant's business data. The repository layer still
filters by tenant_id explicitly wherever the tenant is already known (e.g.
listing a tenant's active sessions).

**8. ID generation.** `github.com/google/uuid` is already an *indirect*
dependency (pulled in transitively via testcontainers-go). Promoting it to
direct for tenant/user/role/session ID generation, same reasoning as
bcrypt in decision 5 — no new module enters `go.sum`.

**9. `FORCE ROW LEVEL SECURITY` + non-superuser test role.** Postgres never
applies RLS to superusers, and skips it for a table's owner unless the
table is explicitly `FORCE`d — the testcontainers Postgres module's default
user is a cluster-initializing superuser, so migrating and querying as
that same role would make every RLS assertion a false positive (caught by
hand: the first version of `TestNoContextZeroRows` passed for the wrong
reason, then failed once seeding was fixed, until `FORCE ROW LEVEL
SECURITY` and a non-superuser test role were added). `kernel/tenancy`'s RLS
tests now migrate as the superuser, then create an ordinary `NOSUPERUSER
NOBYPASSRLS` role and run all assertions through that connection instead —
the way a real application is meant to connect. **This generalizes beyond
tests: WP-0.8's "DB role separation" AC must ensure the production app
never connects as a superuser/table-owner, or RLS is decorative in
production too, not just in a naive test.**

**10. `TIMESTAMPTZ` vs. modernc.org/sqlite auto-parsing.** WP-0.2 deliberately
chose `TIMESTAMPTZ` over `TIMESTAMP` for Postgres (docs/notes/WP-0.2-decisions.md,
point 9). `modernc.org/sqlite` only auto-parses a TEXT timestamp column back
into `time.Time` when its declared type is exactly `DATE`/`DATETIME`/`TIMESTAMP`
— not `TIMESTAMPTZ` — so scanning directly into `*time.Time` silently broke on
SQLite. Rather than forking the schema per dialect, added `storage.Time` /
`storage.NullTime` (`kernel/storage/storage.go`): `sql.Scanner` wrappers that
accept either a real `time.Time` (Postgres) or the raw string SQLite hands
back, parsed with the same format the driver writes
(`time.Time.String()`'s layout). Single portable schema, no per-dialect
migration split for this.

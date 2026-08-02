# WP-1.12 decisions — TOTP enrollment

**Status: implemented (2026-08-01).** Branch `wp-1.12`. Four things changed
between approval and merge; each is marked in place, with the original reasoning
kept so the correction is legible:

- **§3** takes `rsc.io/qr` ([ADR-020](../adr/ADR-020-qr-encoding.md)) instead of
  hand-rolling an encoder — decided before implementation.
- **§4** now requires the password to *enroll*, not only to disable. The plan
  exempted the upgrade path; that left a stolen session able to enroll an
  attacker's authenticator and lock the owner out permanently.
- **§6**'s lockout does both writes in SQL. The first cut computed the counter
  in Go, which lost updates under concurrency and could clear the lock.
- **§9**'s flag is `CarriesCredentials`, covering the idempotency *request
  fingerprint* as well as the response body — the disable body carries a
  password, and an unsalted hash of it is an oracle.

§6's "cut line" was not taken: the lockout landed with the WP. The last three
came out of the integrity review; the invariant table below also corrects an
INV-T3 citation that should always have read INV-T2.

WP-1.12 closes the P2 finding in [phase-1-review.md](phase-1-review.md): TOTP can
be *validated* but never *enrolled*. `kernel/identity/totp.go` has shipped a
hand-rolled RFC 6238 implementation since WP-0.3, `users` has carried
`totp_secret` / `totp_enabled` / `totp_last_counter` since migration `0004`, and
the login route has accepted a `totp` field since WP-1.5 — but nothing in the
product ever writes `totp_enabled = true`. `EnableTOTP` and `GenerateTOTPSecret`
have **zero non-test callers**. The authentication level actually available today
is password-only, which makes milestone M1's own bullet ("password+TOTP
acceptable for dogfood") untrue. That is why this WP gates M1.

## Invariants touched

No new catalog entries and no `TestRequired` flips — all four were already
enforced and tested. This WP adds paths that must not break them.

| Invariant | Why it applies here |
|---|---|
| **INV-T1** | A new tenant-scoped table (`totp_recovery_codes`) with `tenant_id` first in the primary key and an RLS policy, plus every new query filtering on tenant explicitly. A recovery code minted in tenant A must not authenticate anyone in tenant B, on either dialect. |
| **INV-T2** | The enrollment routes are **authenticated**, so the `Action.Public` allowlist in `internal/app/routes_integrity_test.go` does not change — and a test asserts it did not. The structural fence in `auth_integrity_test.go` ("no authentication decision in `internal/app`") is *extended*, not weakened: see §7. |
| **INV-T2** (second half) | Enrollment and disablement act only on the caller's own account. No `{id}` is accepted from the request, for the same reason `DELETE /api/v1/sessions/current` accepts none — a path parameter here would be an authorization decision made by a URL. *(Corrected during implementation: this was filed under INV-T3, which is about permission floors and value domains not being widened by overlays, plugins or agents. Nothing in this WP touches INV-T3.)* |
| **INV-T4** | Every transition (`totp.enroll.started`, `totp.enabled`, `totp.disabled`, `totp.recovery.used`) writes an `audit_log` row carrying actor, action and timestamp. This is the WP's explicit acceptance criterion. |

## Ambiguities resolved

### 1. Where the pending secret lives: in `users`, disabled, until confirmed

`users.totp_secret` + `totp_enabled = false` is *already* the exact shape of a
pending enrollment. `StartTOTPEnrollment` writes the secret with
`totp_enabled = false`; `ConfirmTOTPEnrollment` validates a code against that
stored secret and flips the flag. The login path ignores a secret whose
`totp_enabled` is false, so a pending enrollment changes nothing about how the
account authenticates until it is confirmed. **Confirm-before-enable falls out
of the existing schema with no new state.**

`EnableTOTP` (secret + flag in one `UPDATE`) is therefore the wrong primitive and
is split into `SetPendingTOTP` and `ConfirmTOTPEnrollment`. It has no non-test
callers, so nothing breaks.

The alternative — return the secret from `enroll` and have the client send it
back with the confirmation code — was rejected: it makes the *client* the source
of a credential the server is about to trust, and a caller could then confirm a
secret of their own choosing.

**Confirm must persist the matched counter.** `ValidateTOTP` takes
`lastCounter` and rejects a replayed step, but `totp_last_counter` is NULL during
enrollment, so without this the code used to confirm would still be usable to log
in for the remainder of its 30-second step. `ConfirmTOTPEnrollment` writes
`totp_enabled = true` and `totp_last_counter = <matched counter>` in one
statement.

**No enrollment TTL.** Considered and declined: a `totp_pending_at` column plus
expiry buys little, because confirming requires an already-authenticated session,
and starting a new enrollment overwrites the pending secret (which is the real
mitigation for "I walked away from a half-finished enrollment"). Named here so
the omission is a decision rather than an oversight.

**A user with no password cannot enroll.** `StartTOTPEnrollment` refuses when
`password_hash` is empty. TOTP in this product is the second factor *of the
password path*; docs/08 is explicit that on the OIDC path the IdP authenticates,
and MFA there is the IdP's job. Enrolling TOTP on a password-less account would
produce an account that can neither use its second factor nor re-authenticate to
remove it.

### 2. Recovery codes: new table, SHA-256, 120 bits, single-use by `UPDATE … WHERE used_at IS NULL`

**Storage.** New table at migration `0039`, RLS at `0040`:

```sql
CREATE TABLE totp_recovery_codes (
    tenant_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    code_hash  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id, code_hash),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id)
);
```

`tenant_id` first in the only index, per the tenancy commandment; the composite
FK reuses `idx_users_tenant_id` exactly as `sessions` does. Rows are *marked*
used rather than deleted, so "3 of 10 remaining" is derivable and a consumed code
leaves a trace. `DisableTOTP` deletes the rows outright — a re-enrollment must
not inherit codes from the previous secret.

**Hashing: SHA-256, not bcrypt.** bcrypt exists to make *low-entropy,
human-chosen* secrets expensive to guess. A recovery code is neither: it is 120
bits from `crypto/rand`. Three concrete reasons SHA-256 is the right choice here
and bcrypt is the wrong one:

1. **The entropy carries the load.** 2^120 is not reachable at any hash rate. A
   work factor tuned for an 8-character password buys nothing against a value
   that was never guessable.
2. **A deterministic hash makes verification a single indexed lookup.** bcrypt's
   per-row random salt would force "fetch all N rows for this user and bcrypt the
   candidate against each" — 10 × ~100 ms on every login attempt that presents a
   recovery code, which is both a performance cliff and a self-inflicted DoS
   vector on an unauthenticated route.
3. **The repo already does exactly this for the same class of secret.**
   `kernel/identity/session.go` `hashToken` stores SHA-256 hex of a 256-bit random
   bearer token. Recovery codes are bearer tokens with a longer shelf life;
   treating them differently would be inconsistent without being safer.

No per-row salt, for the same reason `hashToken` has none: there is no
precomputation to defeat against a 120-bit random space, and a deterministic
hash is what makes (2) possible. Binding tenant+user *into* the hash input was
considered and dropped in favour of keeping them in the `WHERE` clause (where
the primary key enforces them) plus an integrity test proving user A's code does
not work for user B — same guarantee, simpler function.

**Format.** 10 codes per enrollment. Each is 15 bytes of `crypto/rand` →
24 characters of unpadded RFC 4648 base32, rendered in six dash-separated groups
of four (`K7QX-2M4A-…`). 15 bytes rather than 16 because 120 bits divides evenly
into base32 characters and 128 does not; the base32 alphabet (`A–Z`, `2–7`) has
no `0`/`1`/`8`/`9` to confuse with `O`/`I`/`B`/`g`. Input is normalised —
uppercased, dashes and whitespace stripped — before hashing, so a user may type
it in any of the forms they might reasonably type it in.

**Single-use is enforced by the database, not the application.** Consumption is:

```sql
UPDATE totp_recovery_codes SET used_at = ?
WHERE tenant_id = ? AND user_id = ? AND code_hash = ? AND used_at IS NULL
```

and the caller requires `RowsAffected() == 1`. Two concurrent logins presenting
the same code therefore cannot both succeed on either dialect — the loser sees
zero rows affected and gets `ErrInvalidCredentials`. Checking `used_at` in Go and
then updating would be a check-then-act race; this is not.

`totp_recovery_codes` is deliberately **not** append-only and is not added to
`kernel/integrity`'s `AppendOnlyTables`: marking a code used is the whole point,
and the append-only grants exist for `events` and `audit_log`, which this is not.

### 3. The QR code: `rsc.io/qr`, server-side, PNG data URI — and an ADR to say so

**Reversed 2026-08-01, before implementation.** This section originally decided
a hand-rolled in-tree encoder (`kernel/qr`: byte mode, level M, versions 1–10,
full mask selection, plus a four-angle test suite including a decoder written in
the test file). It is now **[ADR-020](../adr/ADR-020-qr-encoding.md): take
`rsc.io/qr`.** The original reasoning is kept below the decision, because the
argument that overturned it is the useful part.

The roadmap asks the enrollment endpoint to issue an `otpauth://` URI **and** a
QR. A QR encoder is not in the standard library and is not in `go.mod`, so this
is a real decision under the no-new-dependency rule either way — the ADR is a
deliverable regardless of which way it lands.

**Decision: `rsc.io/qr`.** `identity.TOTPQRDataURI` is
`qr.Encode(uri, qr.M)` plus a base64 wrapper. Zero transitive dependencies; it
is the encoder `rsc.io/2fa` uses for this identical purpose; stable since 2015,
which for a pure function over a frozen ISO standard is what finished looks
like.

**Why the original argument was wrong.** It ran: ADR-019 declined a JOSE
library, so decline this one too. That inverts:

- ADR-019's case was about a **parser** — untrusted input, a real
  talked-into-accepting-the-wrong-thing failure mode. A QR encoder parses
  nothing. It takes a string this server generated and emits a bitmap. Its
  failure mode is "the code will not scan": loud, local, and caught immediately.
  Same conclusion in ADR-019 (don't import a parser you can write safely), the
  opposite conclusion here.
- The **costs were never comparable**. ADR-019's verifier is a few hundred lines
  of comparisons. A conforming encoder is Reed–Solomon over GF(256), interleaved
  block layout, alignment and version-information tables, and ISO/IEC 18004 mask
  penalty scoring — ~600 lines that must be *correct*, plus a test harness large
  enough to prove it without a reference implementation to diff against. Around
  1000 lines of non-TOTP code inside a TOTP work package, owned forever.

Optimizing a dependency count by paying in code-to-get-right is the wrong trade.
The no-new-deps rule is satisfied by writing the ADR, not by the ADR always
saying no.

**Delivery** (unchanged from the original plan): server-side, PNG, data URI in
the enroll response.

- *Server-side rather than client-side*: "every capability must be reachable via
  API/MCP" — a CLI or MCP client enrolling a user must be able to display a QR
  without reimplementing the encoder. Rendering it in TypeScript would put the
  encoder somewhere the API cannot reach.
- *PNG rather than SVG*: the current CSP is `img-src 'self' data:`, so both work
  and both are inert inside `<img>`. PNG is chosen because the encoder already
  emits one and it removes the "is a data: SVG a script vector" conversation
  from every future security review.
- *Data URI in the JSON body rather than a second `GET` route*: a route serving
  the image would be a cacheable GET returning a credential, and would need its
  own no-store handling and its own authz check. One response, one credential,
  one lifetime.

The QR is rendered with `alt=""` (decorative) beside the base32 secret shown as
selectable text. The secret **is** the accessible equivalent of the image and is
needed anyway for manual entry, so this is both the correct a11y answer and one
that passes the axe-core gate without inventing a description of a bitmap.

**URI shape:**

```
otpauth://totp/LastERP%20(acme):alice@example.com
    ?secret=…&issuer=LastERP%20(acme)&algorithm=SHA1&digits=6&period=30
```

Issuer is `LastERP (<tenant id>)` rather than bare `LastERP`, because a user with
accounts in two tenants would otherwise get two indistinguishable entries in
their authenticator. The tenant **id** is used, not the display name: it is what
the user already types on the login screen, and it does not change under them
when someone renames the tenant. `algorithm`/`digits`/`period` are written
explicitly even though they are the defaults — some apps honour them, and being
explicit prevents one defaulting to SHA-256 against our SHA-1 implementation.

### 4. Re-authentication for disable: password **and** a second factor, where a recovery code counts as the second factor

The threat is a session an attacker has, not a password an attacker has: an
unattended logged-in browser or a stolen session must not be able to strip MFA
off the account. So a session alone is not enough — disable demands the password
again.

Requiring a *current TOTP code* as the only second factor would be wrong, and
would deadlock the exact user who most needs to disable: someone whose
authenticator device is gone. That is what recovery codes are for. So:

> **Disable requires the account password AND either a current TOTP code or an
> unused recovery code.**

Both are checked by the existing provider. The handler resolves the session's
user, and `(*PasswordTOTPProvider).Reauthenticate` runs the *same* decision
`Authenticate` runs — password verification, TOTP validation with step burning,
or recovery-code consumption — against a user id instead of an email. It shares
one unexported helper with `Authenticate` so there is still exactly one
implementation of "is this person who they claim to be". The semantics fall out
for free: because TOTP is by definition enabled when disable is called, the
second factor is never optional.

**Enrollment also requires the password — reversed during implementation.** The
plan exempted it: enabling a second factor is a security *upgrade*, and making
an upgrade harder than the status quo is how MFA adoption stays low. Review
found the hole that argument misses. An attacker holding only a **session**
enrolls their own authenticator, confirms it, and keeps the recovery codes. The
legitimate owner still knows the password but now cannot disable anything —
disable demands a second factor they do not have, and administrator reset is
deferred (below). That is an absorbing state: password-only has no equivalent,
so the threat table's "no worse than the password-only status quo it replaces"
was simply wrong.

The account is password-only at the moment of enrollment, so requiring the
password asks for the only factor that exists. It costs one form field and
closes a session-theft path into permanent account loss. Adoption friction is a
real concern, but it does not outrank "a stolen session can lock you out of your
own account forever".

Order of checks in the handler matters: already-enabled (409) and no-password
(422) are answered *before* the credential check, so a caller is not asked to
prove who they are only to be told the request was never going to work. Neither
leaks anything — `GET /api/v1/me/totp` reports both to the same caller.

### 5. Recovery codes are accepted at login through an explicit field, not format sniffing

`POST /api/v1/sessions` grows a `recovery_code` field beside `totp`.
`identity.Credentials` grows a matching `RecoveryCode`. The alternative —
accepting a recovery code in the `totp` field and telling the two apart by shape
(six digits vs. dashed base32) — was rejected: an authentication path should not
contain a guess, the UI is clearer when the user chooses deliberately, and the
audit trail can then distinguish the two.

Every rejection stays `ErrInvalidCredentials`, so a wrong recovery code, a wrong
TOTP code, a wrong password and an unknown user remain a single undifferentiated
401 with an identical body — the property `TestLoginFailuresAreIndistinguishable`
already pins, extended to cover the new field.

The public-route allowlist is unchanged: this adds a field to an existing
allowlisted route, not a route.

### 6. Second-factor brute force is throttled per user, because the gateway rate limit does not stop it

The gateway's default budget is 100 req/s with a burst of 200, keyed by client
IP for public routes. `ValidateTOTP` accepts a ±1 step window, so three of the
10^6 codes are live at any instant: at 100 attempts/second an attacker who
already has the password expects to hit a valid code in **under an hour**, from
one IP, and faster from several. A second factor with that property is decorative.

This gap exists today, but today nothing can turn TOTP on, so nothing is behind
it. This WP is what puts accounts behind it, so the throttle lands here:

- two columns on `users` (migration `0041`): `totp_failed_attempts INT NOT NULL
  DEFAULT 0`, `totp_locked_until TIMESTAMPTZ`;
- a failed second factor (TOTP *or* recovery code) increments; the 10th
  consecutive failure sets `totp_locked_until = now + 15 minutes`;
- any success resets the counter to zero and clears the lock;
- while locked, second-factor verification returns `ErrInvalidCredentials`
  without evaluating anything — same undifferentiated 401, no "you are locked
  out" oracle.

The counter is per user and only advances after the password has already
verified, so it is not a lever for an anonymous attacker to lock accounts they
cannot otherwise touch. Ten attempts tolerates typos and a badly synced phone
clock; fifteen minutes is short enough not to need an unlock path and long
enough to reduce a one-hour expected search to centuries.

**Cut line:** if the reviewer judges this out of scope for an enrollment WP, it
can be dropped to a follow-up without touching anything else in the plan — it is
one migration and one file. The threat notes below then have to say the second
factor is brute-forceable, which is why it is proposed here. *(Not taken: it
landed with the WP.)*

**Both writes happen in SQL, against the stored row — corrected during
implementation.** The first cut read `TOTPFailedAttempts` into Go, added one,
and wrote the result back as an absolute value. That is a lost update, and both
of its failure modes hand the win to exactly the attacker this control exists to
stop — one who has the password already and runs attempts in parallel inside the
gateway's budget:

- N concurrent attempts all read *k* and all write *k+1*, so the counter never
  reaches ten and the lock never fires;
- worse, any request holding a pre-lock snapshot writes `totp_locked_until` back
  to `NULL`, **unlocking** an account the instant it locked.

The fix is `SET totp_failed_attempts = totp_failed_attempts + 1` with the lock
derived in a `CASE` over the same expression, and `ELSE totp_locked_until` so an
existing lock is preserved rather than cleared. It is the same check-then-act
trap §2 avoided for recovery codes, one control over — and the reason it
survived the first pass is that the lockout had only sequential tests while the
recovery-code path had a concurrency one. It now has both.

### 7. The `internal/app` authentication fence is extended, never widened

`internal/app/auth_integrity_test.go` fails the build if any non-test file in
`internal/app` contains `VerifyPassword(` or `ValidateTOTP(`. Every function this
WP adds that makes an authentication decision lives in `kernel/identity`, so the
handlers call `identity.ConfirmTOTPEnrollment`, `identity.StartTOTPEnrollment`
and `provider.Reauthenticate` and the fence is satisfied without special-casing.

The fence is additionally **strengthened**: `ConsumeRecoveryCode(` joins
`passwordPrimitives`. A recovery code is a credential, and verifying one in the
composition root would be the same mistake the fence was built to catch, one
credential type later.

### 8. Route shape: `/api/v1/me/totp/*`

| Route | Write | Body → Response |
|---|---|---|
| `GET /api/v1/me/totp` | no | → `{enabled, pending, recovery_codes_remaining}` |
| `POST /api/v1/me/totp/enroll` | yes | → `{otpauth_uri, secret, qr_png}` |
| `POST /api/v1/me/totp/confirm` | yes | `{code}` → `{enabled: true, recovery_codes: […]}` |
| `POST /api/v1/me/totp/disable` | yes | `{password, totp?, recovery_code?}` → 204 |

`me` rather than `users/me`: `users` is a resource segment a future metadata
object could plausibly claim, and `me` is one nothing can. `Object: ""` — these
are self-service account operations, not capability-gated module objects, exactly
like `DELETE /api/v1/sessions/current`. All three writes carry an
`Idempotency-Key` like every other write (ADR-009).

Re-enrolling while already enabled is a **409**, not a silent re-issue: rotating
a live second factor is disable-then-enroll, and the audit trail should say so.

### 9. `Action.CarriesCredentials` — because `idempotency_keys` would otherwise be the longest-lived copy of every secret the API touches

This one is a genuine finding, not a preference. `kernel/api`'s `handleWrite`
persists the **verbatim response body** of every 2xx write into
`idempotency_keys.response_body`, and that table has no TTL and no cleanup. A
`Write: true` route that returns recovery codes would therefore write them, in
plaintext, into a second table, permanently — defeating the WP's own
"stored hashed" requirement by a side channel nobody would think to look at.

**And the request side is worse — found in review, after the plan was written.**
The same table stores `request_fingerprint = SHA-256(method ‖ path ‖ body)`,
unsalted. The disable body is `{"password":"…","totp":"123456"}`; method and path
are constants and a TOTP code adds about twenty bits. That row is an offline
password oracle roughly two orders of magnitude cheaper than the bcrypt in
`users.password_hash` — a hash is not plaintext, so the grep that catches the
response side would never have flagged it. It is the first `Write: true` route
whose body carries a password (login is `Public`, which is not a write).

Fix: `kernel/api.Action` grows **`CarriesCredentials bool`**, covering both
columns. `response_body` is finalized **empty**, so a replay returns the status
and `Idempotent-Replayed: true` and nothing else — the right semantics for a
show-once credential, which must not be re-revealed. `request_fingerprint` is
computed over method and path only, never the body. All three writes set it.

The cost of the fingerprint change is that two different bodies under one key
read as a replay rather than a 409 on these routes. Acceptable where it is set:
the request *is* the credential, so a caller reusing a key with a different body
is retrying, not issuing a new command.

The alternative — marking these routes `Write: false` to dodge the wrapper —
was rejected: it drops a repo-wide rule ("all writes take idempotency keys") to
work around a storage detail, and it leaves the next credential-minting endpoint
(API keys, WP-3.0 secrets) to rediscover the same trap.

Two tagged integrity tests cover it on both dialects: one drives the full
enroll → confirm flow over HTTP and greps `idempotency_keys.response_body` and
`audit_log.changes` for the TOTP secret and every issued recovery code; the
other recomputes the fingerprint a body-inclusive implementation would have
stored and asserts no such row exists, while checking that the body-free
fingerprint *is* there — idempotency still works, it just does not commit to a
body. It is the same shape
as the WP-3.0 acceptance criterion ("grep the logs, event store and an export for
the plaintext and find none").

## Deferred, with reasons

- **Regenerating recovery codes without disabling.** Not in the acceptance
  criteria, and it does not solve the problem it looks like it solves: it would
  need the same re-authentication as disable, so a user with no authenticator and
  no unused codes still could not use it. Disable-then-re-enroll issues a fresh
  set and is the supported path.
- **Administrator MFA reset.** The genuine gap (see threat notes) needs a user
  administration surface, and there is none — `lasterp bootstrap` is the only
  path that has ever created a user, and there is no user CRUD API at all. That
  is a WP, not a corner of this one.
- **Encrypting `totp_secret` at rest.** It has been plaintext since WP-0.3 and
  this WP does not change that. Field-level encryption is specified in docs/08
  and blocked on the secrets vault (**WP-3.0**), exactly as the OIDC client
  secret was in [WP-1.9-decisions.md](WP-1.9-decisions.md) §2.
- **WebAuthn.** docs/08 lists it beside TOTP. Different WP, different ADR.
- **Revoking a user's other sessions on a TOTP change.** Defensible either way;
  not asked for, and the account-settings screen has no session list to make it
  legible. Named so the omission is deliberate.

## Threat notes

| Threat | Control |
|---|---|
| Attacker with a stolen/borrowed session disables MFA | Disable requires the password **and** a second factor (§4). A session alone proves nothing about the person holding it. |
| Attacker enrolls *their own* authenticator on a victim's account | **Enrollment requires the password** (§4, reversed during implementation), so a stolen session alone cannot do it. The original plan exempted enrollment and the threat note here claimed it was "no worse than the password-only status quo" — that was wrong: an attacker who enrolled would take the recovery codes too, and the owner could then never disable the factor, which password-only has no equivalent of. Once TOTP is enabled a second enrollment is a 409 and needs a disable first, which needs the second factor. Every transition is audited (INV-T4). |
| Online brute force of the 6-digit second factor | Per-user lockout after 10 consecutive second-factor failures, 15 minutes (§6). Without it, a 100 req/s budget breaks a ±1-step TOTP window in under an hour. The counter and the lock are both written in SQL against the stored row: computing them in Go would let concurrent attempts lose updates and even clear the lock, which is the shape review caught and a concurrency test now pins. |
| Recovery codes brute-forced online | Same lockout path; 120 bits of entropy makes online guessing irrelevant anyway. |
| Recovery codes recovered from a database read | Stored as SHA-256 of a 120-bit random value (§2). Not reversible, not guessable, and — via `CarriesCredentials` (§9) — not sitting in `idempotency_keys` either, in the response body or hashed into the request fingerprint. Proven by two tagged tests. |
| Account password recovered from `idempotency_keys` | The disable request body carries the password, and `request_fingerprint` is an unsalted SHA-256 of it — a cheaper oracle than the bcrypt hash it is meant to protect. Credentialed routes fingerprint method and path only (§9). |
| Recovery code replayed | Single-use enforced by `UPDATE … WHERE used_at IS NULL` + `RowsAffected == 1`, so concurrency cannot double-spend one (§2). |
| TOTP code replayed within its step | `totp_last_counter` is advanced on every successful validation, including the one that confirms enrollment (§1) — otherwise the confirmation code stays live as a login code for the rest of its window. |
| Cross-tenant use of a recovery code | `tenant_id` first in the primary key, an RLS policy on Postgres, and an explicit tenant predicate on every query (the only guard on SQLite). Tagged test on both dialects (INV-T1). |
| User enumeration through the new field | A wrong recovery code returns the same undifferentiated 401, with a byte-identical body, as a wrong password or an unknown user (§5). |
| CSRF on the enrollment routes | Unchanged from every other write: `SameSite=Strict` session cookies plus the mandatory `Idempotency-Key` header, which a cross-origin form post cannot set. |
| The secret leaking through the browser | Delivered once over an authenticated response, rendered into the DOM, never written to `localStorage` and never in a URL. The bundle has no inline script and CSP is `script-src 'self'`. |
| **Permanent MFA lockout** (no authenticator *and* no unused recovery codes) | **Not fully mitigated.** Ten codes are issued, the settings screen shows the remaining count and warns at three or fewer, and the codes are presented once with an explicit copy/download step. Beyond that the only recovery today is operator access to the database. Accepted, with an administrator reset deferred above — it is the same posture as GitHub's and Google's (account recovery is a support process), but it is a real product gap and should not be discovered in a postmortem. |
| Operator reading a TOTP secret from the database | Not mitigated; unchanged from WP-0.3. Blocked on WP-3.0 (see Deferred). Anyone with that access can also read `password_hash` and forge a session row. |

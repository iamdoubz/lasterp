# WP-3.0 — Secrets vault: decisions

Interpretation of [docs/11-ROADMAP.md](../11-ROADMAP.md) WP-3.0 against
[docs/08-SECURITY-MULTITENANCY.md](../08-SECURITY-MULTITENANCY.md) §Data protection
(kernel envelope encryption, KMS/age key source, capability-gated `secrets.get`) and
[docs/19-DATA-INTEGRITY.md](../19-DATA-INTEGRITY.md). Written before the code, per CLAUDE.md.

## 0. Scope

In: `kernel/secrets` (envelope encryption, tenant-scoped storage, rotation), a management API
that never returns plaintext, the invariant and its tests, a rotation CLI subcommand.

Out, deliberately: the plugin host's `secrets.get` host function (WP-3.1 — the roadmap already
says INV-X1 lands there, and there is no plugin runtime to gate today); per-tenant OIDC provider
configuration (§9); migrating TOTP secrets to the vault (§9); a KMS key source (§2).

## 1. The envelope is per secret, because rotation is a re-wrap

AC: *"key rotation re-wraps without re-encrypting the payloads."* That sentence fixes the shape.
Each secret row carries its own 32-byte DEK, generated at write time, used once to seal the value
with AES-256-GCM, and stored **wrapped** by the deployment KEK. Rotation unwraps the DEK with the
old KEK and re-wraps it with the new one: one short column changes, the ciphertext is untouched.

A per-tenant or per-deployment DEK would need a key table, a cache and a lifecycle of its own, and
would buy nothing — the payloads are small and few. Per-row is the lazy shape that satisfies the AC
literally.

AES-256-GCM from `crypto/aes` + `crypto/cipher`; nonce is 12 bytes of `crypto/rand` prepended to
the ciphertext. **Both seals bind their context as AAD**: the value's AAD is
`tenant_id | name`, the wrapped DEK's is `tenant_id | name | key_id`. So a ciphertext lifted from
one tenant's row into another's — by a SQL bug, a restore, or a hand-edited backup — fails to open
rather than decrypting into the wrong tenant. That is defence in depth under INV-T1, not its
primary enforcement, which stays RLS.

## 2. One key source ships: a key file. No new dependency

docs/08 says "keys in KMS/age file for self-host". `KeySource` is the interface that sentence
describes; `FileKeySource` is the implementation that ships:

```
# $LASTERP_SECRETS_KEYFILE, mode 0600
current = 2026-08-a
2026-08-a = <base64 32 bytes>
2026-07-a = <base64 32 bytes>   # kept until rotation has drained it
```

**Not the age file format, and no `filippo.io/age`.** age's value is its recipient/identity model
for *sharing* encrypted files between people; a deployment KEK is read by one process and shared
with nobody. Pulling in a dependency for a format we would use as a bag of bytes fails the
no-new-runtime-dependencies rule for nothing. If someone wants literal age files — to reuse an
existing age-managed secret store — that is another `KeySource` implementation and an ADR, not a
change here.

**No KMS source either.** A KMS source means an AWS/GCP/Azure SDK — dependencies measured in
hundreds of packages — for a deployment shape nobody runs yet. The interface is the deliverable;
the cloud implementation lands with the cloud deployment that needs it. `KeySource` is deliberately
tiny (`KeyID`, `Wrap`, `Unwrap`) so that implementation is a file, not a project.

## 3. No key source configured means no vault, loudly

If `LASTERP_SECRETS_KEYFILE` is unset the server still starts, the secrets routes still exist, and
every one of them fails with a problem+json saying the deployment has no key source. It does
**not** generate a key file on first use.

A silently generated KEK is discovered at restore time, in the worst hour of the year, by someone
who has a database backup and no key. Making the operator place one file is a smaller cost than
that, and it is a documented deployment step rather than a surprise.

## 4. There is no endpoint that returns a secret

The API is write, list-metadata, delete:

- `PUT /api/v1/secrets/{name}` — set a value (`secret:manage`)
- `GET /api/v1/secrets` — names, description, key id, timestamps, actor. **Never values.**
- `DELETE /api/v1/secrets/{name}` — remove (`secret:manage`)

Commandment 2 ("everything is an API") is satisfied: every *capability the product offers a user*
is reachable. Revealing a stored credential is not one of them. The person who set the secret
already has it; the server uses it on the tenant's behalf. A reveal endpoint converts the vault
into a credential-exfiltration API reachable with one stolen session, which is the exact outcome
docs/08 §Data protection exists to prevent. `TestNoSecretRevealEndpointExists` enumerates the live
mux and the action table and asserts no route returns secret material — the same structural shape
as WP-2.3b's `TestNoSyncWriteEndpointExists`.

Consequence: a mistyped secret is overwritten, never inspected. Accepted.

## 5. Reading is a server-internal capability, authorized by naming the reader

`secrets.Get(ctx, db, tenant, name, reader)` requires a non-empty `Reader{Kind, ID}` — today
`module:oidc`, tomorrow `plugin:com.acme.commission-calc` — and writes an `audit_log` row
(`object=secret, action=read`) in the same transaction as the read. So AC *"reading a secret is
authorized and audited (INV-T2/T4)"* is met by the only authorization that exists today: a reader
must identify itself, and an anonymous read is refused rather than logged.

The capability *grant* half — checking the reader's manifest actually lists this secret name —
needs a manifest, and manifests arrive with WP-3.1. `Get` takes a `Grants` predicate now so WP-3.1
supplies the plugin manifest check without touching the call sites, and the built-in `AllowAll` is
used only by first-party callers inside the server. INV-X1 stays `TestRequired: false` with its
existing note; this WP does not claim it.

## 6. A secret is not a metadata object, so it cannot leak downstream

`secrets` is a kernel table, not a `metadata` object, and nothing registers it with `crudObjects()`.
That is what keeps it out of the change feed, out of `GET /api/v1/sync/changes`, out of the client
replica and out of every report export — structurally, not by a filter someone must remember. The
AC's grep test asserts this end to end: write a known plaintext, then search the audit log, the
event store, the change feed, a hydration snapshot and a report export for it.

## 7. New invariant: INV-K1

Nothing in the catalog covers "this value must never be stored in the clear" — INV-T1 is
cross-tenant reads, INV-T4 is attribution. WP-3.0 is invariant-bearing code, so per docs/19 it
registers its invariant rather than borrowing one:

> **INV-K1** Secret material is never persisted, logged, emitted or replicated in plaintext: every
> stored secret is ciphertext under a per-secret DEK wrapped by the deployment KEK, and no path
> outside `kernel/secrets` returns it.

Layer: `storage` (the ciphertext is what is in the table) with a `LayerPipeline` component in the
route surface. `TestRequired: true` from this WP. Catalog letter K = key/secret material; **P** is
left free for docs/20's privacy invariants.

## 8. Rotation is per tenant, driven by a CLI

`secrets.Rotate(ctx, db, tenant, source)` re-wraps every row whose `key_id` is not the source's
current key, and reports how many it moved. `lasterp secrets rotate` loops the tenants table. It is
resumable by construction: the predicate is the work list, so a crash halfway leaves the rest
selectable on the next run.

`ciphertext` and `nonce` are asserted byte-identical across a rotation, or the AC's "without
re-encrypting the payloads" is a claim rather than a fact.

## 9. Two known consumers stay on their current storage, and are named here

- **TOTP secrets** are plaintext at rest (docs/08 §AuthN, "blocked on the secrets vault below").
  They are per-*user*, not per-tenant, and the vault is a small keyed store, not a column
  encryptor. Moving them means either a row per user in `secrets` (a name-spacing decision with a
  user-lifecycle question attached — what deletes them when a user is deleted) or a
  `secrets.Seal`/`Open` pair used as a column codec. Both are real work with their own migration
  and their own tests, and neither is in this WP's ACs. **Owner: the WP that lands column-level
  field encryption**; recorded in the roadmap so it stops living only here.
- **The OIDC client secret** (`LASTERP_OIDC_CLIENT_SECRET`, one provider per deployment —
  WP-1.9-decisions.md §2). Per-tenant OIDC configuration is now *unblocked* — the vault is where
  the client secret goes — but it is a feature with its own routes, discovery cache and login-path
  changes. It is a WP, not a footnote to this one.

The vault therefore ships with no first-party consumer, deliberately. The alternative — bolting one
of the two above onto this WP — is how a 400-line kernel package becomes a 2000-line PR that
touches the login path.

## 10. Bytes are stored base64 in TEXT columns

Postgres `BYTEA` and SQLite `BLOB` differ in both DDL and driver round-tripping, and the storage
adapter has no byte-column precedent to follow. Base64 in a `TEXT` column is dialect-neutral,
survives the conformance suite unchanged, and costs 33% on values measured in tens of bytes. The
plaintext-grep AC is unaffected: base64 of a plaintext is not the plaintext, and the test searches
for both forms.

# ADR-021: Replica at-rest encryption is a native-shell control, not a browser one

**Status:** Accepted · 2026-08-04 · WP-2.5 ([decisions](../notes/WP-2.5-decisions.md))

## Context

Two shipped documents contradict each other, and WP-2.5's acceptance criteria sit on the crack.

[docs/08](../08-SECURITY-MULTITENANCY.md) §Data protection says: *"Client replicas: SQLite
encrypted (SQLCipher/OS keystore-derived key); device registration + remote-wipe token honored
at connect (documented limit: an offline stolen device retains data until wipe — **encryption is
the real control**)."* Its threat table names replica encryption as the control for **offline
device theft**. [docs/11](../11-ROADMAP.md) turns that into WP-2.5's AC: *"replica unreadable
without keystore."*

[ADR-017](ADR-017-sync-client-core.md) then fixed the client on `@sqlite.org/sqlite-wasm`'s
**SAH-pool VFS** running in a dedicated worker. Neither document was wrong when written; docs/08
predates the shell decision. Nothing has reconciled them, and the browser is the only shell that
exists before WP-4.7 (Tauri, Phase 4).

## The technical finding

This is not "a cipher library has not been picked yet". Three facts compose:

1. **The VFS is synchronous.** SAH-pool rests on `FileSystemFileHandle.createSyncAccessHandle()`
   and implements `xRead`/`xWrite` synchronously. `crypto.subtle.*` returns Promises. **WebCrypto
   cannot be called inside the VFS at all.** A page-encrypting VFS therefore needs a synchronous
   cipher, which needs key material as bytes in JS memory.
2. **A non-extractable `CryptoKey` cannot help there.** IndexedDB can hold a `CryptoKey` script
   cannot export — the browser's nearest analogue to a keystore — but it is usable *only* through
   the async SubtleCrypto API. The one primitive that resists key theft is the one the VFS cannot
   call.
3. **Origin-scoped is not device-scoped.** The threat docs/08 names is a stolen device. That
   attacker holds the OPFS files **and** the IndexedDB origin data **and** any key the origin can
   reach without user verification. A key that unlocks the replica unattended — which "work all
   day offline" requires — is a key sitting next to the ciphertext.

So a browser replica encrypted with any key the page can obtain silently is not meaningfully
protected against the attacker in the threat model. It would satisfy the sentence in docs/08 and
not the intent behind it.

The one browser primitive that genuinely binds to hardware is the **WebAuthn PRF extension**
against a platform authenticator (TPM / Secure Enclave). It requires user verification —
biometric or PIN — to derive the key, and it is async. It is a real option, and it is rejected
below on cost rather than capability.

## Decision

**Replica at-rest encryption is a control of the native shells, not the browser shell.**

1. **WP-2.5 ships what holds:** device registration, per-device revocation, and remote wipe
   honored on reconnect, on the browser shell, tested end to end.
2. **docs/08 is amended to state the truth.** §Data protection and the threat table stop claiming
   encryption as the browser's control for offline device theft, and name the controls that
   actually apply there: scoped replicas (WP-2.4), remote wipe (this WP), session revocation and
   short-lived tokens, plus the operating system's own disk encryption, which is where full-disk
   protection legitimately lives for a browser client.
3. **The AC moves rather than disappears.** *"Replica unreadable without keystore"* becomes the
   acceptance criterion of a new **WP-4.8** (replica at-rest encryption on the native shells),
   where Tauri supplies an OS keystore and a native SQLite that SQLCipher/SEE can actually be
   built against. It is written down as a numbered work package precisely so it cannot be
   quietly forgotten.

**What is explicitly not decided here:** that browser at-rest encryption is worthless forever.
§Revisit says what changes it.

## Rejected

- **An encrypting VFS replacing SAH-pool.** It replaces rather than wraps, so it re-opens
  ADR-017's COOP/COEP reasoning — the default `opfs` VFS needs `SharedArrayBuffer` and therefore
  cross-origin isolation, which breaks the sandboxed third-party iframes docs/05 §UI extension
  slots and WP-3.6 are built on. It also needs a new runtime crypto dependency, and by finding 1
  it must be a synchronous cipher over an extractable key. Highest cost of the options, and by
  finding 3 it does not deliver the property it costs so much to obtain.
- **Field-level encryption over a PII subset.** Runs at apply time, so WebCrypto and a
  non-extractable key are both available — genuinely cheaper and genuinely better than the VFS
  route. Rejected for *this* WP because it breaks local query and sort on exactly the fields a
  replica exists to serve offline, and because finding 3 still applies: unattended decryption
  means the thief decrypts too. It remains the right shape if a future requirement is "some
  fields must not be readable even by a legitimate local user", which is a different threat.
- **WebAuthn PRF-derived key.** The only option that answers finding 3. Rejected on product cost,
  not security: it puts a biometric/PIN gate in front of opening the replica every session, which
  is in direct tension with milestone M2 ("work all day offline") and with a client whose
  background sync must survive a reload. Worth revisiting when the product has a "high-security
  tenant" posture to attach it to.
- **Shipping the encryption AC as-is with a silent reinterpretation.** The failure mode this ADR
  exists to prevent: editing an acceptance criterion to match whatever got built, without an
  artifact saying so.

## Consequences

- **docs/08's threat table gains an honest row** for offline device theft on the browser shell,
  and loses the claim that encryption is the control there.
- **A wipe destroys unsent work, and a scope purge does not.** WP-2.4 established that a
  revocation purge never touches `_outbox` (the queued command is work no re-fetch reconstructs).
  A wipe is the opposite: the device is presumed hostile, so everything goes, unsent work
  included. Two deliberately different rules, stated so the next reader does not "fix" one into
  the other.
- **The browser's honest posture is written down**: a replica is protected by being *scoped* to
  what the principal may read, by being *wipeable*, and by the OS's own disk encryption — not by
  a cipher inside the page.
- **WP-4.8 is created** and carries the moved AC. WP-4.7 (Tauri) becomes its prerequisite.
- **INV-D1 is added** to the docs/19 catalog (see decisions §4): a wiped device is refused
  service on every authenticated path.

## Revisit if

- **A shell gains a real keystore.** Tauri/mobile do; that is WP-4.8 and is the plan, not a
  revisit.
- **WebAuthn PRF becomes ubiquitous and a tenant posture exists to require user verification on
  replica open.** Then the browser gets real at-rest encryption and this ADR is superseded rather
  than amended.
- **A regulatory requirement lands that names encryption-at-rest on the client** irrespective of
  whether it defeats the stated attacker. Compliance obligations are not always threat-model
  obligations, and docs/20 is where that would surface first.

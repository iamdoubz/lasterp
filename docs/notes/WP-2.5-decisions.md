# WP-2.5 — Device security: decisions

Roadmap line: *"Device security: device registration, replica encryption, remote wipe. AC: wipe
honored on reconnect; replica unreadable without keystore."* Design:
[docs/08](../08-SECURITY-MULTITENANCY.md), [docs/04 §Downstream 4](../04-SYNC-ENGINE.md),
[ADR-004](../adr/ADR-004-sync-model.md), [ADR-017](../adr/ADR-017-sync-client-core.md),
[docs/19](../19-DATA-INTEGRITY.md).

## 0. The encryption half is settled in an ADR, not here

The premortem (2026-08-03) flagged this WP as the one not to open cold, because docs/08 promises
keystore-derived replica encryption and ADR-017's browser shell cannot provide it. That is
resolved in **[ADR-021](../adr/ADR-021-replica-at-rest-encryption.md)**: at-rest encryption is a
native-shell control, docs/08 is amended to say what actually protects a browser replica, and the
AC *"replica unreadable without keystore"* moves to a new **WP-4.8** rather than being reworded to
match whatever got built.

This WP therefore delivers the half that holds: **device registration, per-device revocation, and
remote wipe honored on reconnect.**

## 1. Registration is implicit at session issue; the API manages, it does not enrol

`sessions.device_id` has existed since WP-0.3 as a free string the client supplies, and
`RefreshSession` already binds refresh to it (`ErrDeviceMismatch`). What is missing is that a
device is not a *thing* — there is no row, so there is nothing to name, revoke or wipe.

So `IssueSession` upserts a `devices` row for `(tenant, device_id)` and stamps `last_seen_at`.
Registration is not a separate call the client must remember to make:

- A device that can hold a session is a device worth tracking, and any enrolment step the client
  could skip is one that leaves an untracked replica — the exact thing this WP exists to control.
- It needs no client change to start working, so every existing session (and the test fixtures)
  gain a device row without a migration of behaviour.

The API surface is therefore *management*, not enrolment: list devices, revoke one, wipe one. That
satisfies "every capability reachable via API/MCP" for the capability that actually exists — an
administrator acting on a device.

**Labelling is not built.** The `label` column exists because an admin list of opaque UUIDs is
much less useful than one showing "Dan's MacBook", but nothing writes it: a real label has to come
from the client at login, which is a client change with no consumer yet. The column defaults to
empty and the shipped list renders it as such. A `LabelDevice` helper was written and then deleted
rather than left uncalled — an exported function with no caller is a promise the next reader
believes.

## 2. The wipe signal rides the auth path, because every other design can be skipped

A wipe must reach a client that may never call any particular endpoint again. Three candidate
carriers were considered and two rejected:

- **A polling endpoint** (`GET /api/v1/sync/device`) — the client has to choose to call it. A
  compromised or merely old client that skips it keeps its replica forever. A security control
  that depends on the subject's cooperation to be delivered is not one.
- **A field on the scope or feed response** — better, since WP-2.4 made `/sync/scope` part of
  every reconnect, but it only reaches clients that sync. A tab left open reading cached data
  never learns.
- **The authenticator** — chosen. `identity.ValidateSession` resolves the session, and the device
  behind it is checked in the same breath. A wiped device is refused on **every** authenticated
  path, so there is no request it can make that both succeeds and skips the check.

Mechanically this follows the `ErrAuthUnavailable` precedent already in `kernel/api/gateway.go`: a
distinguished sentinel error (`ErrDeviceWiped`) that the guard renders as its own response rather
than collapsing into the generic 401.

## 3. Why the wipe is distinguishable when `ErrSessionInvalid` deliberately is not

`identity.ErrSessionInvalid` is documented as *"deliberately undifferentiated so callers can't
distinguish 'wrong token' from 'right token, revoked'"* — a token oracle is a real leak and that
design stands.

A wipe is the one case that must be told apart, and the reason is not convenience: **a client that
cannot distinguish a wipe from an expiry does not delete anything.** It signs the user out and
leaves the replica on disk, which is precisely the outcome the control exists to prevent. So the
401 carries `type: "device-wiped"`.

What that discloses is bounded and acceptable: the recipient already holds a token for the device
in question, and the fact being disclosed is "this device was wiped" — which is the message we are
deliberately trying to deliver. It reveals nothing about *other* devices, users or tokens, and it
is reachable only by presenting a session that is otherwise valid.

**A wipe therefore does not revoke the device's sessions.** If it did, `ValidateSession` would
fail first and the client would see an ordinary 401 with nothing to act on. The device keeps a
session that can do exactly one thing: learn that it has been wiped.

## 4. Delivery is recorded; deletion is not claimed

The server stamps `wipe_delivered_at` the first time it refuses a request from a wiped device.
That is an honest name for what is known. **Nothing can prove a remote client deleted anything** —
the disk is not ours — so the column records that the instruction was *delivered*, and the admin
UI says so. A column called `wipe_confirmed_at` would be a claim the system cannot support.

This lands one new invariant, **INV-D1**: *a device marked wiped is refused on every authenticated
path.* It is registered in the docs/19 catalog with `TestRequired: true`, since the enforcement
ships in this WP.

It is a property, not a wish. The test walks the action table, adds the generic CRUD routes — which
are registered straight onto the mux and so are invisible to that scan, the same blind spot
WP-2.3b's `TestNoSyncWriteEndpointExists` records — and probes every one of them over real HTTP,
requiring a 401 carrying `device-wiped`. It first asserts the device could reach those routes
*before* the wipe, so a refusal afterwards cannot be passing for an unrelated reason.

Public routes are excluded, and that is the invariant's own wording rather than a convenience: a
login route must work without a session by construction, so a wiped device reaching one proves
nothing — it never authenticated. The one public route that genuinely needs the check is
`RefreshSession`, and it gets it explicitly (§9).

## 5. A wipe destroys unsent work; a scope purge does not

WP-2.4 §5 established that a revocation purge never touches `_outbox` or `_conflicts`, because the
queued command is the user's own work and no re-fetch reconstructs it.

**A wipe inverts that, deliberately.** The device is presumed to be in the wrong hands, so
everything goes: replica tables, bookkeeping, outbox, conflict tray, cached metadata, and the OPFS
pool files themselves. Losing unsent work is the *point* — it is data on a device that should not
have data.

The two rules sit a few lines apart in the same subsystem and say opposite things, so both carry a
comment naming the other. Getting them backwards in either direction is a serious bug: a purge
that ate the outbox loses a user's work, and a wipe that spared it leaves the thief a copy.

**This is the one sanctioned exception to INV-S4**, and it is called out rather than glossed.
INV-S4 says a rejected command is surfaced to the user and never silently dropped; a wipe drops
queued commands that reached no terminal state. The reconciliation is that INV-S4 is a promise to
*the user of a device*, and a wipe is an administrator's statement that this device has no
legitimate user. There is nobody on it to surface anything to. The catalog note for INV-S4 is
amended to say so, so that a future conservation test failing on a wiped client is read as the
documented exception rather than a fresh bug — and so that nothing else may claim the exemption
without amending the catalog too.

## 6. The client must recognise the wipe before the drain interprets the 401

`kernel/api/gateway.go` already warns that 401 is *"the one status this product's clients act on
destructively"* — `api/client.ts` signs the user out, and the outbox drain files queued work as
rejected rather than retrying (INV-S4).

A `device-wiped` 401 arriving mid-drain must therefore short-circuit the whole sync cycle rather
than be read as a per-command rejection. It is thrown as a distinct `DeviceWipedError` from the
transport, caught above the drain, and it triggers the wipe. Filing conflicts for commands on a
device that is about to be erased would be work spent writing rows into a database being deleted.

## 7. Wiping the OPFS pool, not just the tables

Deleting rows leaves the pages on disk. The SAH pool exposes `wipeFiles()` on its util object,
which is what actually returns the storage to the browser; the replica is closed first so the
exclusive access handles are released.

Best-effort by necessity: a wipe that cannot complete (the tab is closing, the pool is held) must
not leave the device believing it succeeded, so the local wipe is idempotent and re-attempted on
next open — the server keeps refusing, so the client keeps being told.

## 8. Not built here

- **Replica at-rest encryption** — ADR-021, deferred to WP-4.8 on the native shells.
- **Per-device scopes.** WP-2.4 §10 noted a scope is per *principal*. Narrowing a scope further
  per device is expressible now that devices are entities, but nothing asks for it and it would be
  a second entitlement dimension with no consumer.
- **Device attestation / trusted-device enrolment flows** (an admin approving a new device before
  it may sync). Real, and a different feature: this WP makes devices visible and revocable, which
  is the prerequisite for that rather than a substitute.
- **Push-based wipe.** The wipe is honored at the next authenticated request, which is what docs/04
  §Downstream 4 and docs/08 both specify ("honored at connect"). A truly offline device is
  unreachable by construction — that is the documented limit, and ADR-021 is where the honest
  statement of what does protect it now lives.

## 9. Two gaps found by re-reading the invariant against the code

Neither surfaced from a failing test, which is worth recording as the honest provenance — both were
found by checking INV-D1's wording against the route table and against what SQLite actually does.

**`RefreshSession` is a public route**, so "every authenticated path" does not reach it. Without a
check there a wiped device mints a fresh access token whenever the old one expires — each useless
for data, but each extending a session that should be over, indefinitely. It is now refused there
too, with the typed error rather than a bare one: an expiring client hits refresh before it hits
anything else, so in practice this is often where a wipe is first *delivered*. Revoked devices are
stopped on the same path and in `IssueSession`, because an administrator's decision must not be
undone by the most routine thing a client does.

**Deleting rows does not reclaim the bytes.** `wipeReplica` empties every table, and SQLite frees
those pages into the file's freelist rather than returning them to the filesystem — so after a
"wipe" the data was still sitting in the OPFS file, readable by whoever has the machine, which on a
stolen device is the entire threat. `adapters/opfs.ts` `wipePool()` (`SAHPoolUtil.wipeFiles()`) is
what actually returns the storage, and the worker calls it after closing the store, since the pool
holds its access handles exclusively. The core cannot do this itself: reclaiming storage is an
adapter concern that the synchronous port deliberately does not expose.

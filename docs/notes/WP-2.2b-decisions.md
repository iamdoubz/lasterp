# WP-2.2b decisions — client replica

Roadmap: *"PR-B (WP-2.2b) client replica — SQLite-WASM/OPFS schema generation from
`/meta/objects` (never a hand-written mirror, per ADR-006 + ADR-017), hydration, incremental apply
on the WP-2.6 core, and the worker boundary ADR-017 §Consequences requires. INV-S3 flips to
TestRequired in PR-B."*
Design: [docs/04](../04-SYNC-ENGINE.md) · [ADR-004](../adr/ADR-004-sync-model.md) ·
[ADR-017](../adr/ADR-017-sync-client-core.md) · [ADR-006](../adr/ADR-006-metadata-customization.md) ·
[docs/19](../19-DATA-INTEGRITY.md) · [WP-2.2-decisions.md](WP-2.2-decisions.md).

This file supplements [WP-2.2-decisions.md](WP-2.2-decisions.md), which planned both PRs. It
records only what building B settled that planning A could not, plus one revision to that file.

---

## 1. Money mirrors the server's column, and this revises WP-2.2 §6

**WP-2.2 §6 says the replica stores money as "minor units + currency as two columns". That is not
what the server does, and B follows the server.**

`kernel/metadata/ddl.go:32` maps `money`, `decimal`, `percent` and `currency` to a single `TEXT`
column holding the exact string. §6 was written from the commandment ("integer minor units, never
float") and reached a representation the server had already decided differently.

Mirroring wins for a reason that is structural rather than a matter of effort:

- **The AC is row-for-row equality against the server's projection.** A replica whose physical
  shape differs from the server's needs a translation layer inside the comparison, and a bug in
  that layer is indistinguishable from convergence. Identical columns make the oracle a direct
  comparison with nothing in between.
- **It is the same argument ADR-017 used to reject Rust** — a second representation of a
  metadata-derived type is a drift surface in the component whose entire job is not drifting. A
  second *physical layout* is the same defect one level down.
- **Nothing in Phase 2 queries money locally.** Reports are server reads (WP-2.2 §9). Splitting the
  column buys local aggregation that no caller wants yet.

The commandment is not weakened: no float appears on either side, and the exact string round-trips
byte-for-byte. If client-side money aggregation ever lands — ADR-017 §Revisit's "the core stops
being orchestration" — that is when to split, and it is then a replica schema migration, not a
correctness fix.

## 2. `/meta/objects` must say which objects are replicable — a WP-2.2a gap B exposes

`listMetaObjects` filters by capability enablement only; it does not say whether an object is
`crud` or `event_sourced`. But `/api/v1/sync/snapshot` 404s for event-sourced objects, because
`resolver.newResolver` skips anything `metadata.NewCRUD` refuses.

So a client generating its replica from `/meta/objects` — which is the whole instruction — has no
way to know which of those objects it can hydrate. Three ways to close it:

- **Probe: treat a snapshot 404 as "not replicable."** Conflates "event-sourced" with "unknown
  object" and "module disabled", and makes an error the control flow.
- **Hardcode the event-sourced list client-side.** A hand-written duplicate of metadata, forbidden
  by CLAUDE.md and by ADR-006 — and the exact thing this WP exists to avoid.
- **Add `persistence` to the `metaObject` projection.** One field, information the caller
  legitimately needs, and the server already holds it. **Chosen.**

This is the same shape as WP-2.2 §1a: a pointer-target choice made with no consumer to check it
against, corrected by the first consumer. B is that consumer for `/meta/objects`.

## 2a. Overlay fields become real columns in the replica

The server splits a schema in two: core fields are physical columns, overlay fields live in a
`custom_fields` JSON blob (ADR-006, `GenerateDDL`). The reason is sharing — *one* physical table
serves every tenant, and two tenants may overlay the same core object differently, so an
overlay-added field cannot be a fixed column.

**A replica has no such constraint.** It holds one tenant on one device, so its effective schema is
unambiguous and every field — core or overlay — becomes a real column. This is not a divergence
from the server's data, it is the same data without a constraint that does not apply.

It also settles a question `/meta/objects` cannot answer: `metaField` does not expose
`FromOverlay`, so a client could not reproduce the core/overlay split even if it wanted to. Storing
every effective field as a column is the only shape the published metadata actually supports, and
it is the right one.

Consequence for §8's comparison: the oracle reads the server's *record* (which merges the blob back
in), not its columns, so the two line up field-for-field.

## 3. Three field types stay out of the replica, and the exclusion is asserted

WP-2.2 §6 excludes `table`, `computed` and `file`. Confirmed as written, with the mechanism
recorded: `computed` is a real `FieldType` (`schema.go:53`) that `columnType` maps to `TEXT`, so
excluding it is a client-side choice rather than something the server's DDL already refuses. The
generator therefore *skips* those three explicitly and a test asserts the skip, because an
exclusion that happens by falling through a switch is one refactor away from silently becoming an
inclusion.

## 4. Hydration bookkeeping is per object, and the feed cursor is not

`_sync_state` (WP-2.6's prototype) holds one cursor for one scope. Hydration needs more: a snapshot
pages per object, and a client interrupted halfway through hydrating must resume that object rather
than restart it.

So: `_sync_state` keeps the scope cursor (unchanged, still keyed by scope so WP-2.4 is additive),
and a second table `_hydration` tracks `(object, after_id, done)`. The feed cursor recorded at the
**first** snapshot page is written once and not moved by later pages — WP-2.2 §4's rule, and the
reason a mid-hydration row shift repairs itself.

Hydration is complete when every replicable object is `done`. Only then does the apply loop start;
a client that folds feed entries into a half-hydrated replica would be applying updates to rows it
does not have, and `INSERT … ON CONFLICT` would silently manufacture partial ones.

## 5. Multi-tab: detect and surface, do not coordinate

The SAH-pool VFS holds an **exclusive** access handle. A second tab opening the same replica gets
`SQLITE_CANTOPEN`, and the honest reading of that today is a blank screen — the design has never
modelled a second tab, and an ERP user having two open is ordinary.

Leader election (Web Locks + a shared worker proxying the replica) is real work and is not in this
WP's AC. What is not acceptable is failing opaquely. So B **detects** the condition and surfaces it
as a distinct, rendered state ("this replica is open in another tab"), and WP-2.3 owns coordination
— which it must, because an outbox with two writers is a correctness problem and not merely a
usability one.

<!-- ponytail: single-writer replica, detected not coordinated. Web Locks leader
     election lands with WP-2.3, where two writers stop being a UX question. -->

## 6. `POOL_CAPACITY` is measured, not guessed

ADR-017 §Consequences left this to WP-2.2: the SAH pool pre-allocates a fixed slot count and fails
with `SQLITE_CANTOPEN` beyond it rather than growing. The prototype's `16` was chosen for a
469-line spike with one table.

B derives it: a test opens a fully generated replica, exercises hydration and apply, and asserts
the pool's file count against the configured capacity with the headroom stated as a constant. A
number that fails loudly in CI when the replica grows a file is worth more than a comment saying it
was picked deliberately.

## 7. Storage persistence is requested; the denied path is WP-2.3's to harden

`web/src/sync/` never calls `navigator.storage.persist()`. Browsers evict OPFS under storage
pressure without warning, and iOS evicts on inactivity.

For **this** WP the cost of eviction is bounded: the replica is read-only (WP-2.2 §9), so an evicted
replica is a re-hydration, not lost work. B therefore requests persistence, records the grant
outcome where the UI can show it, and re-hydrates cleanly when the replica is gone.

**The unbounded case arrives with WP-2.3**, whose `_outbox` holds commands the server has never
seen — the one thing in the client that no amount of re-fetching reconstructs. Stated here rather
than left implicit, because it is cheap now and structural later: WP-2.3 must decide what happens
when persistence is *denied* before it accepts a single offline write.

## 8. The convergence test must be able to fail (the AC, sharpened)

WP-2.2 §8 gives the AC two tests. There is a failure mode in the first one worth naming, because it
would produce a green INV-S3 over a broken replica:

**Materialised rows are current server state (WP-2.2 §2), and the oracle is current server state.**
A harness that syncs to quiescence and then compares can pass by construction — the final pass
re-fetches and overwrites whatever an earlier bug damaged. Such a test measures that `SELECT`
equals `SELECT`, and it would not have caught WP-2.1's audit-pointer defect either.

So the property test carries two anti-tautology provisions:

1. **A mutation check.** A test drops a deterministic subset of feed entries between the server and
   the client and asserts the property test **fails**. A convergence suite that still passes with
   5% of the feed deleted is not testing selection, and this makes that falsifiable rather than a
   matter of opinion.
2. **A seeded fixture the generator did not create.** Rows that exist before the run — including
   ones with null optional fields and archived-before-first-sync rows — so the passing state space
   is not a subset of what the generator happens to produce.

Neither is gold-plating: they are the difference between INV-S3 meaning "converges" and INV-S3
meaning "the harness and the code agree about one query".

## 9. Invariants

- **INV-S3** flips `Note: lands with WP-2.2` → `TestRequired: true`. Proven by §8, not asserted.
- **INV-S5** — unchanged from WP-2.1; the apply loop depends on it and the prototype's ordering
  cases (`suite.ts`) come along and grow.
- **INV-T1** — hydration and materialisation are tenant-scoped reads; the browser pass runs as one
  tenant and the Go-side property test asserts a second tenant's rows never reach the replica.
- **INV-T5** — values arriving at the replica conform to the object's effective schema. The client
  does not re-validate business rules (the server is the referee) but must not silently coerce: a
  value that does not fit its declared column surfaces as an error, and the apply transaction rolls
  back rather than storing a rounded one.

Not claimed: **INV-S1/S2/S4**, which are upstream and land with WP-2.3.

## 10. Deferred, with reasons

- **Everything upstream** — outbox, optimistic apply, replay, conflict tray: WP-2.3. This replica is
  read-only and says so in its API surface.
- **Per-field dirty tracking.** WP-2.3's field-level master-data merge (docs/04 §Conflict policy)
  needs to know which fields a client changed. Nothing writes locally in this WP, so building it
  here would be a column with no writer. What B *does* owe 2.3 is that adding it is additive: the
  generated column set is produced by one function with an explicit list, so a `_dirty` sidecar is
  a new generated column and not a rewrite of the generator.
- **Multi-tab coordination** — §5.
- **Event-sourced objects** (`JournalEntry`) — WP-2.2 §9, unchanged.
- **Live push** — polling converges; the notifier exists and wiring a transport is WP-2.1 §7's
  standing deferral.
- **Replica encryption** — WP-2.5. Flagged here because it is not merely later, it is *unresolved*:
  docs/08 §Client replicas commits to "SQLite encrypted (SQLCipher/OS keystore-derived key)", and
  ADR-017 fixed the client on the official `sqlite-wasm` SAH-pool build, which has neither a cipher
  nor an OS keystore — and an encrypted VFS replaces SAH-pool rather than wrapping it, so adopting
  one re-opens ADR-017's COOP/COEP reasoning. B does not resolve this and must not pretend to; it
  is recorded so WP-2.5 does not discover it cold at the end of the phase.

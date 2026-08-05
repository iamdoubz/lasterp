# WP-2.7 — Offline-first screens: decisions

Roadmap line: *"Make the replica the read path for metadata-driven screens and route `ObjectForm`
writes through the outbox."* Added by [phase-2-review.md](phase-2-review.md) P1.2. Prerequisite
deliverable, written first and approved before this file: [M2-airplane-mode-script.md](M2-airplane-mode-script.md).

## 1. Scope is the two objects that exist, and that is not a compromise

`internal/app/app.go` `crudObjects()` publishes **Account** and **Contact** to `/meta/objects`, and
the replica generates its schema from that list (ADR-006). Those two objects are the offline
universe. Invoice, Receipt and Period are declared `persistence: crud` in their modules but are not
in that list; `JournalEntry` is `event_sourced` and excluded by design (WP-2.2 §9).

Confirmed with Dan before planning: **WP-2.7 ships the seam for the objects that exist.** Widening
it is additive by construction — an object added to `crudObjects()` replicates with no client change
at all, which is the payoff of metadata-driven schema generation — and it is *not* free, because
invoice writes go to bespoke routes (`/api/v1/invoices`) while the outbox builds paths from
`api.ResourcePath`, which is `strings.ToLower(objectName)` and yields `/api/v1/invoice`. The outbox
cannot address the invoicing routes today. That is a finding, recorded here and in the script's
§Not covered, not something this WP quietly works around.

**GL posting stays online-required** (docs/04 §Offline capability matrix). Queueing it would be
*safe* — the server revalidates against current state, so a post into a closed period is refused
into the tray and INV-F3 is never at risk — but it is a product decision about a documented default,
with INV-F6 document numbers allocated only at acceptance, and a demo is not a reason to make it.

## 2. The replica is the read path. There is no API fallback

Screens read from the replica whether or not there is a network. Not "replica when offline, API when
online".

A fallback is the obvious kindness and it is wrong here:

- **Two read paths that can disagree is the divergence INV-S3 exists to prevent.** A fallback does
  not remove the disagreement, it hides it — the screen silently looks right while the replica is
  wrong, which is the one failure mode the whole phase was built to make impossible.
- **It would mean the offline path is only exercised in the demo.** A path that runs once a release
  is a path that is broken and nobody knows. Commandment 4 says the network is an optimization; a
  client that prefers the network when it has one has not implemented that, it has described it.
- The unavailable cases are already modelled and do not need a fallback: a second tab gets
  `ReplicaLockedError` rendered as a distinct state (WP-2.2b §5), and a replica that has never
  reached the server has no schema and correctly has nothing (script §Preconditions 2).

The cost accepted: a screen can show data slightly older than the server. `sync()` runs on mount,
after every write and on reconnect, so the window is a poll interval and the badge shows the cursor.
An ERP list being seconds stale is ordinary; an ERP list disagreeing with the replica underneath it
is not.

## 3. One write path, and the command id *is* the idempotency key

`ObjectForm` currently mints a `writeKey` per mounted form — *"one key per mounted form, not per
submit: a retry after a network blip is the same logical write and must not create a second
record"*. That reasoning is right and survives; only the identifier changes. The form now mints a
`command_id`, which **is** the `Idempotency-Key` when the drain replays it (WP-2.3 §1), so the
property the comment describes is now enforced by the outbox rather than by the form.

Creates mint the row id client-side (UUIDv7, WP-2.3 §2), so the row the user sees offline is the row
the server ends up with and step 7 of the script — editing a row that has never reached the server —
has something stable to target.

## 4. Pending is surfaced from `_pending`, not recomputed

The replica already records which rows carry unsent changes, in the `_pending` sidecar, and
`outbox.ts` already exports `pendingRows(store, object)`. The worker gains one protocol command that
returns those ids; the list and detail screens flag them. Nothing new is computed and nothing is
stored twice — a second source of truth for "is this row pending" is exactly the drift this
subsystem keeps refusing to introduce.

`_pending` is a sidecar rather than a column precisely so the INV-S3 convergence oracle stays a
direct equality (WP-2.3 §3); reading it here does not change that.

## 5. What this WP does *not* get to change

- **No server changes.** If a screen needs something the server does not offer, that is a finding
  for the next WP, not a patch here. This WP is a seam.
- **No new invariants.** It touches INV-S1/S3/S4 (it is now a *human* driving the outbox those
  invariants govern) and INV-T5 (values are conformed on the way into the replica), all already
  `TestRequired` and enforced in the core. Nothing flips.
- **No weakening of the conflict tray.** It already renders the stored command and the server's
  problem+json rather than joining to the replica (WP-2.5 §6), which is what makes script steps 14
  and 15 possible at all.

## 6. The AC is the script, and "no step reading the API" is asserted mechanically

The acceptance criterion is [M2-airplane-mode-script.md](M2-airplane-mode-script.md) executed end to
end in a real browser over OPFS with the network off. Two things make that a test rather than a
demo:

- **The offline assertion is enforced, not observed.** The Playwright context fails the test if any
  request to `/api/v1/` is issued while the script is in its offline phase. "No step reading the
  API" is the one claim a human running the demo cannot check by looking.
- **Volume is part of the criterion.** The script says a meaningful day is 20+ mutations interleaved
  with reads and at least one reload, because one-of-each takes ninety seconds and passing it would
  reproduce exactly the hollowness the premortem warned about. The spec executes at that volume.

## 7. What the script found that no unit test could

The roadmap entry made the script a prerequisite deliverable on the theory that writing it first
surfaces decisions instead of discoveries. It did better than that: **running it found four defects
that every existing test suite passed over**, three of them fatal to M2 and one of them fatal to the
whole of Phase 2 in production.

1. **The CSP forbade WebAssembly, so the replica could never start in the shipped app.**
   `script-src 'self'` without `'wasm-unsafe-eval'` makes the browser refuse
   `WebAssembly.instantiate`, and the sync worker dies on load. Every headless test passes (Node,
   no CSP) and so does the browser OPFS pass (its own harness, no Go server), so nothing caught it.
   This is why the phase-2 review found the screens did not use the replica: **they could not
   have.** Fixed by adding `'wasm-unsafe-eval'`, which permits WASM compilation only and does not
   restore `eval()` — reaching for `'unsafe-eval'` here would have been a real weakening.
2. **Reloading the tab offline failed at the document request.** The replica held the data; the
   *application* was still fetched from the server every load. Fixed with a hand-written service
   worker (`web/public/sw.js`) — network-first with a cache fallback, so a connected user always
   runs current code and a disconnected one runs the last code they saw. It never caches
   `/api/v1/`, because a cached API response would be the second data path §2 refuses, arriving
   through the back door.
3. **Offline, the app signed the user out.** The mount probe treated *any* failure as
   unauthenticated, so losing the network logged you out — at exactly the moment the offline client
   becomes useful. Now only a real 401 does, which is the same distinction
   `kernel/api/gateway.go` already draws server-side with `ErrAuthUnavailable`: "could not check"
   is not "not authenticated". It has to hold on both ends.
4. **The navigation could not render offline**, because it is built from `/meta/objects`. It now
   falls back to the schema the replica cached. That is not the §2 fallback: `_meta` holds exactly
   what that endpoint last returned, so there is one source and one cache of it. A nav built from
   stale *schema* shows the wrong menu; a list built from stale *data* shows the wrong money, and
   only one of those gets a fallback.

Two of the four are one-line changes. All four were invisible to 149 unit tests, a headless
convergence harness with four virtual clients under partitions and crashes, and a browser OPFS
pass — because none of those runs the product.

## 8. Known follow-ups this WP does not fix

- **A refused write shows a developer's sentence.** `conform` rejects a required enum at enqueue
  time (INV-T5) with `sync: Contact.kind does not conform to its schema: "" is not one of […]`.
  Before this WP the same mistake reached the server and came back as localized problem+json. The
  form is `noValidate` and does not check required-ness client-side, so this is a real, if small,
  regression in the error the user reads — recorded rather than left to be discovered. The fix is
  client-side required-field validation in `ObjectForm`, which is a form-rendering change and not
  a sync one.
- **The e2e suite tests a stale bundle.** `scripts/e2e-server.sh` rebuilds the web app only when
  `dist/index.html` is absent, so a source change silently tests the previous build. It cost
  several confusing runs here. Worth a `--rebuild` flag or a checksum, and worth knowing about
  before trusting a green e2e run after a UI change.
- **`useRecords` loads an object's whole table** to render one row on the detail and edit screens.
  It is deliberate — it shares the list's cache, so the two can never disagree, and the query is
  local SQLite — but it is O(rows) per screen and the right shape for a large tenant is a
  `get(object, id)` command. Marked here rather than pre-optimised.

# M2 — the airplane-mode script

The acceptance criterion for **Milestone M2, "work all day offline, sync perfectly"**, written as a
user's day rather than a developer's seam. Prerequisite deliverable of
[WP-2.7](../11-ROADMAP.md), written **before** any of its code, because the premortem's test for a
hollow milestone is whether the script is short — and that is only answerable by writing it.

Run it in a real browser against a real server, with the network actually off (DevTools offline, or
the machine's radio). Every step names what the user does and what must be true. **No step may read
the API.**

## What the replica can hold today, and why the script looks like this

`internal/app/app.go` `crudObjects()` publishes exactly two objects to `/meta/objects`:
**Account** and **Contact**. The replica generates its schema from that list
(`replicableObjects`, ADR-006), so those two objects *are* the offline universe right now.
Invoice, Receipt and Period are registered `persistence: crud` in their modules but are not in
`crudObjects()`, and `JournalEntry` is `event_sourced` and excluded by design
(WP-2.2-decisions.md §9).

So this script is a **chart-of-accounts and CRM day**, which is not a compromise dressed up: it is
the first two rows of docs/04's own offline-allowed list — *"create/edit drafts, record CRM data …
view everything in scope"*. What it deliberately is **not** is an invoice; see §Not covered.

## Preconditions

1. A tenant with a handful of Contacts and Accounts, and a user holding `Contact:{read,create,update}`,
   `Account:{read,create,update}` and `sync:read`.
2. Sign in **online** once, on the device under test, and let the first sync finish: the status badge
   reads hydrated, cursor non-zero. This is the "issued laptop" moment — a replica that has never
   reached the server has no schema and no data, and that is correct behaviour, not a bug to design
   around.
3. Confirm `navigator.storage.persist()` was granted. If it was denied the shell says so and the
   outbox is capped (WP-2.3-decisions.md §6) — note it and continue; the script must still pass, with
   the cap as the documented limit.

## The offline day

**Go offline now.** Nothing below may touch the network.

| # | The user does | Must be true |
|---|---|---|
| 1 | Reloads the tab | The app opens. Lists render from the replica. No spinner that never resolves, no error page. |
| 2 | Opens the Contacts list | Every contact that was in scope at step 2 of the preconditions is listed. |
| 3 | Opens one contact | Detail renders in full from the replica. |
| 4 | Creates 3 new contacts | Each appears in the list **immediately**, flagged pending. |
| 5 | Edits 2 existing contacts | The edit is visible immediately, flagged pending. |
| 6 | Creates an Account | Appears immediately, flagged pending. |
| 7 | Edits that new Account before it has ever reached the server | Works. This is the case a provisional-id design breaks: the row the user sees offline is the row the server ends up with (WP-2.3-decisions.md §2), so a second edit has something stable to target. |
| 8 | Reloads the tab again | All of steps 4–7 survive. The pending count is unchanged. This is the step that separates a replica from a cache. |
| 9 | Deletes one of the contacts created in step 4 | Disappears from the list; its queued create/delete pair resolves to nothing on the server rather than a create followed by a 404. |
| 10 | Tries a deliberately invalid edit — an email the server will refuse | Accepted locally. The client does not re-validate business rules; the server is the referee (ADR-004). |
| 11 | Checks the status badge | Shows the pending count and that it is offline. The user is never in doubt about whether their work has left the building. |

**A meaningful day is more than one of each.** If steps 4–7 are executed once each, the whole thing
takes ninety seconds and the premortem's charge of hollowness stands. Execute them at volume — 20+
mutations across both objects, interleaved with reads and at least one reload — because that is
what "all day" means and it is where a cap, a leak or an ordering bug actually shows up.

## Meanwhile, on the server

While the device is offline, another user (or curl) changes **the same field of one of the contacts
edited in step 5**. This is the conflict the tray exists for, and without it the reconnect below
proves only the easy path.

## Reconnect

**Go back online.**

| # | Happens | Must be true |
|---|---|---|
| 12 | The client drains, re-shapes, hydrates, pulls | In that order (WP-2.3 §4 as refined by WP-2.4 §4). |
| 13 | The valid work lands | All creates and edits from steps 4–9 are on the server, exactly once. Re-running the sync changes nothing. |
| 14 | The invalid edit from step 10 is refused | It appears in the **conflict tray**, showing the user's own values and the server's problem+json — not a client-side approximation of it. |
| 15 | The colliding edit surfaces | The tray shows it. The user resolves or discards, deliberately. |
| 16 | The badge settles | Pending 0; conflicts equal exactly the number of genuinely refused commands. Nothing has silently vanished. |
| 17 | Reload once more | The replica matches the server's projection for every in-scope object. |

## The property behind the demo

Steps 13 + 16 together are the milestone in one sentence: **every command the user issued offline
ended up either on the server or visible in the tray, and none of them ended up in neither.** That
is INV-S1 and INV-S4, which the simulation harness already proves headlessly with four virtual
clients under partitions and crashes — this script is the same property with a person in the loop,
which is the part a harness cannot check.

## Not covered, and why

- **Invoices.** Not in `crudObjects()`, so not in `/meta/objects`, so not in the replica. Adding
  them is additive by construction — the schema is metadata-driven, so an object added to that list
  replicates with no client change — but it is not free: invoice writes go to bespoke routes
  (`/api/v1/invoices`, plural) while the outbox builds paths from `ResourcePath`, which is
  `strings.ToLower(objectName)` and yields `/api/v1/invoice`. The outbox cannot currently address
  the invoicing routes at all.
- **Posting to GL offline.** docs/04 lists it as **online-required by default** and this script does
  not relax that. It would be *safe* to queue — the server revalidates against current state and a
  post into a closed period would be refused into the tray, so INV-F3 is never at risk — but it is a
  product decision about a documented default, with INV-F6 document numbers allocated only at
  acceptance, and it should not be made to make a demo look better.
- **Multi-tab.** A second tab is blocked by the exclusive VFS handle and rendered as a distinct
  state (WP-2.2b §5). Opening one is not part of the script; confirming the message is honest is.
- **A device that has never synced.** Correctly has nothing. Out of scope here, in scope for the
  shell's first-run copy.

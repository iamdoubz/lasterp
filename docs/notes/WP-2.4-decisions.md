# WP-2.4 — Scope management: decisions

Roadmap line: *"Scope management: role-based scope computation, re-shape on change, revocation
purge. AC: entitlement-change scenarios."* Design: [docs/04 §Downstream 4](../04-SYNC-ENGINE.md),
[docs/08](../08-SECURITY-MULTITENANCY.md), [ADR-004](../adr/ADR-004-sync-model.md),
[ADR-006](../adr/ADR-006-metadata-customization.md), [docs/19](../19-DATA-INTEGRITY.md).

Three seams were left open for this WP and are closed here: WP-2.1 §4 ("scope tagging is a seam;
WP-2.4 owns the engine"), WP-2.2 §3 ("per-device scope narrowing is WP-2.4, unchanged"), and
WP-2.3 §4 ("the purge runs last — this WP fixes the contract 2.4 must honour").

## 1. A scope is the set of object kinds the actor may read — nothing narrower, yet

`change_feed.scope_key` is the object name (WP-2.1 wrote the column and said so). A scope is
therefore a **set of scope keys**, and role-based computation is: the objects this actor holds a
`read` grant on, intersected with the objects that are replicable at all (a CRUD surface exists)
and whose module the tenant has enabled.

Row-level narrowing — docs/04's *"my region's customers, open documents last 24 months"* — is
**not** built. It cannot be: `authz` has no condition evaluation (`ErrConditionNotSupported`, since
WP-0.3), so there is no expression language for "my region" to be written in, and inventing one
here would be a second authorization system living beside the one the rest of the product uses.
When conditions land, they subdivide these keys rather than replacing them, exactly as WP-2.1
argued for the key itself.

What this *does* close is real: WP-2.1's threat notes conceded that holding `sync:read` meant
seeing the tenant's whole feed, bounded only by pointers being weaker than rows. After this WP the
pointers are filtered too, so `sync:read` is the right to follow *your own* scope rather than the
tenant's.

## 2. The scope has no version; the client diffs the list

docs/04 §Downstream 4 says the server "bumps scope version". A version is a change-detector, and
this client has nothing to detect with it: the reconnect cycle already fetches `/meta/objects`
every time, so fetching a list of a dozen object names costs the same round trip a version check
would and answers the further question ("which ones") in the same breath.

So `GET /api/v1/sync/scope` returns the list, and the client re-shapes by diffing it against what
it holds. The observable behaviour docs/04 specifies — re-shape and purge honoured at the next
connect — is identical; the state that can go stale is one fewer.

A version becomes worth having when a scope can narrow *within* an object (§1), because then the
list no longer changes when the entitlement does. That is the same WP that brings conditions.

## 3. `_hydration` is already the record of what this replica holds

The re-shape needs to know what the replica currently replicates. That is not a new table:
`_hydration` has one row per object this replica has taken (or is taking) a snapshot of, and it is
the only thing in the replica that means "I hold this object". So:

    purge  = _hydration − scope   → delete the object's rows, delete its _hydration row
    adopt  = scope − _hydration   → insert a _hydration row; the next hydrate() fills it

The generated table itself is left in place. Its schema comes from metadata (ADR-006), not from
the scope, and dropping it would mean re-running DDL on the next grant for no gain over an empty
table.

## 4. The re-shape runs after the drain and before the hydrate — a refinement of WP-2.3 §4

WP-2.3 §4 fixed the order as `drain → hydrate → pull`, with "WP-2.4's revocation purge after pull".
The load-bearing half of that is **after the drain**, and the reason it gives is the right one: a
purge that runs first deletes rows that queued commands reference. That is honoured.

"After pull" is not, and the refinement is deliberate:

- The *adopt* half must run **before** `hydrate`, or a newly-entitled object has no `_hydration`
  row for hydration to act on and would not appear until the cycle after next.
- The *purge* half gains nothing from running after `pull`, because the server no longer sends
  out-of-scope rows at all (§1) — there is nothing for a later pull to re-deliver.
- Splitting the two halves across the cycle would mean two passes over the same diff, in two
  places, to preserve a sequencing that buys nothing.

Order is therefore `open → drain → reshape → hydrate → pull`.

The re-shape runs on **every** cycle, not only when the list changed. It is a diff over a dozen
names, and running it unconditionally is what makes the resurrection case self-heal: a command
rejected in cycle *n+1* rolls its row back into the replica (`rollback` restores the pre-image),
which for a revoked object would otherwise reinstate a row the user is no longer entitled to. The
same cycle's re-shape removes it again.

## 5. A purge never touches `_outbox` or `_conflicts`

This is the watch-list item this WP was told to instrument first, and it is the whole answer to
"a purge must never delete a row id referenced by an undrained command".

There are two readings of that rule. The first — *don't delete the replica row while a command
references it* — makes the purge negotiable, and a revocation purge that a queued command can veto
is not a revocation. The second — *don't delete the command* — is the one taken here, because the
command body **is** the user's work and the replica row is a copy of the server's. Losing the row
costs a re-fetch the user is no longer entitled to make; losing the command loses work no re-fetch
reconstructs.

So: the purge deletes rows from generated tables and rows from `_hydration`. It does not issue a
statement against `_outbox`, `_conflicts` or `_pending`, and a test asserts the counts across a
revocation are conserved.

The ordinary path is not even that dramatic. The drain runs first, so a queued command for a
revoked object is *sent* before its rows are purged; the server refuses it with a 403 whose
problem+json is a real explanation, and the drain files it in the tray exactly as it files any
other rejection.

## 6. The tray renders the command, never the row — and now there is a test saying so

The second watch-list item: for an object the user can no longer read, materialisation returns
nothing, so a tray built by joining conflicts to replica rows would render zero rows and look
healthy while work was being lost.

It is not built that way — `_conflicts` carries the command's own `body` plus the server's
problem+json, and `Conflicts.tsx` renders those columns (WP-2.3 §1). That was a consequence of
replaying stored HTTP requests rather than a defence against this case, which is precisely why it
deserves a test rather than a comment: nothing today would notice if a later change made the tray
join to the replica.

`TestTrayShowsWorkForAnObjectTheUserCanNoLongerRead` revokes the entitlement under queued work and
requires the conflict, its body and the server's reason to survive the purge.

## 7. A filtered page must still advance the cursor — two consequences

Filtering the feed breaks two assumptions the unfiltered version could make.

**Server.** `readChanges` set `cursor` to the last *returned* entry. With a filter, a page can be
short or empty while the feed has more, and the client's "a short page means caught up" rule would
stop it dead. So the handler reads the feed's high-water mark **before** the page and reports that
as the resume position whenever the page came back short — which is exactly the condition "nothing
in scope remains at or below it". Taking the high-water first is what makes it safe: an entry
committed after it is above the reported cursor and is seen next time, and an entry below it is
committed already, because the per-tenant ordering lock (WP-2.1 §2, `order.go`) makes id order
equal commit order. That is INV-S5's existing guarantee being *used*, not extended.

**Client.** `applyPage` derived the new cursor from the entries in the page, which is no longer
possible when the entries the cursor covers were filtered out. It now takes the server's reported
`page.cursor` when that is ahead of what it derived, and **throws** when it is behind an entry the
same page just delivered — a resume point behind delivered work is the stranding bug INV-S5 exists
to prevent, and it is better to fail loudly on the client than to advance past it.

## 8. Correction: per-scope locking is not the feed's relief valve

`order.go`'s comment names "a per-scope lock" as the upgrade path when the per-tenant ordering lock
becomes a throughput ceiling. **That sentence is wrong and this WP corrects it.**

Per-scope ordering under a single global `bigint` cursor reintroduces the exact stranding the lock
exists to prevent: two scopes ordering independently means a reader trusting `id > cursor` can
observe id 7 from scope A while id 5 from scope B is still uncommitted, and 5 is then stranded
forever. Making that safe requires per-scope **cursors**, which is not a lock change — it rewrites
WP-2.2a's reader, WP-2.2b's `_sync_state`, WP-2.3's acknowledgement path and INV-S5's own wording.

The comment is amended in place to say so. No behaviour changes here; the point is that the next
person under throughput pressure does not read a one-line aside as a design.

## 9. Invariants

Nothing flips: every INV-S is already `TestRequired`. Touched, with tagged tests:

- **INV-T1 / INV-T2** — the feed and the scope endpoint are authorized read paths; the filter is
  an authorization decision applied to pointers that previously escaped one (§1).
- **INV-S1** — a revocation under queued work loses nothing: the command survives the purge and
  reaches the server or the tray (§5).
- **INV-S3** — convergence now means "converges to the *scoped* projection": a re-shaped replica
  holds exactly the in-scope rows, no more and no fewer.
- **INV-S4** — a rejection caused by revocation is surfaced with the server's own reason (§6).
- **INV-S5** — the filtered cursor still observes every in-scope entry exactly once, in a stable
  order, and never advances past one it did not deliver (§7).

## 10. Not built here

- **Row-level / conditional scopes** (§1) — needs `authz` condition evaluation.
- **Scope versions** (§2) — arrive with the above.
- **Device-level scopes.** A scope is per *principal*, not per device: `sync/scope` is computed
  from the actor. Devices are WP-2.5's subject, and a per-device narrowing is a filter over this
  set rather than a different computation.
- **Server-side purge instructions.** docs/04 says revocation "queues a purge instruction"; the
  client's diff (§2, §3) produces the same effect at the same moment with no queue to keep, ack or
  garbage-collect.
- **Any caching of the computed scope.** It costs one grants query plus one module-enabled query
  per granted object, on a path a client polls, and it is recomputed every time on purpose: a
  stale scope is a principal still being served data after their entitlement was withdrawn, which
  is the single failure this WP exists to prevent. Correct invalidation is a larger thing than the
  query it saves, and there is no measurement saying this is hot — the docs/09 budget covers the
  CRUD routes, not the feed. Marked at the call site with the upgrade path (a request-scoped memo
  on `capability.Enabled`, whose real granularity is the module rather than the object, before
  anything that outlives a request).

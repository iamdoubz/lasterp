// SPDX-License-Identifier: AGPL-3.0-only

// Hydration and the poll loop — the two things that turn the core's apply
// function into a replica that catches up and stays caught up.
//
// docs/04 §Downstream 5: "Initial hydration: snapshot download (paged) at a
// consistent position, then stream from there." Streaming is polling until a
// transport lands (WP-2.1 §7); polling converges, push only changes latency.

import {
  applyPage,
  applyRecord,
  cachedMeta,
  cacheMeta,
  currentCursor,
  indexSchema,
  initReplica,
  SCOPE,
} from "./core.ts";
import type { SchemaIndex } from "./core.ts";
import { drain } from "./outbox.ts";
import { replicableObjects, type MetaObject } from "./schema.ts";
import { MAX_PAGE, type Transport } from "./transport.ts";
import type { Store } from "./port.ts";

/** PAGE_SIZE is the page both hydration and the feed request. It is the
 * server's cap: the round trip dominates and the rows are small. */
const PAGE_SIZE = MAX_PAGE;

interface HydrationRow {
  object: string;
  after_id: string;
  done: number;
}

/** open generates the replica's schema from live metadata and returns the index
 * the apply loop needs. This is the "never a hand-written mirror" step: the
 * tables come from /meta/objects, per tenant, overlays included (ADR-006).
 *
 * When the server cannot be reached it falls back to the copy cached by the
 * last successful open. A read-only replica did not need that — no server means
 * a stale read, and failing was honest. An outbox does: a tab reloaded offline
 * must still accept a write, and it cannot resolve one without knowing the
 * object's fields. A replica that has *never* reached the server has no schema
 * to fall back to and still fails, which is the correct outcome — there is no
 * replica yet. */
export async function open(store: Store, transport: Transport): Promise<SchemaIndex> {
  let objects: MetaObject[];
  try {
    objects = await transport.meta();
  } catch (err) {
    const cached = cachedMeta(store);
    if (cached === null) throw err;
    initReplica(store, cached);
    return indexSchema(cached);
  }

  initReplica(store, objects);
  cacheMeta(store, objects);
  return indexSchema(objects);
}

/** hydrated reports whether every replicable object has finished its snapshot.
 *
 * The apply loop must not start before this is true. A client folding feed
 * entries into a half-hydrated replica applies updates to rows it does not
 * have, and `INSERT … ON CONFLICT` would manufacture partial ones out of an
 * update payload — a divergence that looks like data (WP-2.2b-decisions.md §4). */
export function hydrated(store: Store): boolean {
  const pending = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM _hydration WHERE done = 0`);
  return Number(pending[0].n) === 0;
}

/** hydrate pages every object's snapshot into the replica, resuming where a
 * previous run stopped, and returns the feed cursor to resume from.
 *
 * Paging a table that is being written to can show a mix of old and new rows.
 * That is repaired rather than prevented: every change after the recorded
 * cursor is in the feed and applying it is idempotent, so a row that shifted
 * mid-snapshot is fixed by the first feed batch. The alternative — one
 * repeatable-read snapshot spanning every page — holds a transaction open for
 * the length of a full hydration, which on a large tenant is a long-lived
 * reader in the write path (WP-2.2-decisions.md §4).
 */
export async function hydrate(
  store: Store,
  index: SchemaIndex,
  transport: Transport,
): Promise<number> {
  const pending = store.query<HydrationRow>(
    `SELECT object, after_id, done FROM _hydration WHERE done = 0 ORDER BY object`,
  );

  for (const row of pending) {
    const schema = index.get(row.object);
    if (schema === undefined) {
      // The object vanished from metadata between runs — a module was disabled.
      // Nothing to hydrate, and its table stays until the next re-shape
      // (WP-2.4 owns scope changes; this WP does not delete on its own).
      store.exec(`UPDATE _hydration SET done = 1 WHERE object = ?`, [row.object]);
      continue;
    }

    let after = row.after_id;
    for (;;) {
      const page = await transport.snapshot(row.object, after, PAGE_SIZE);

      store.transaction(() => {
        // The cursor is recorded once, from the first page of the first object,
        // and never moved by a later page. Taking a later page's high-water
        // mark would skip every change committed in between.
        if (!hydrationStarted(store)) {
          store.exec(`UPDATE _sync_state SET cursor = ? WHERE scope = ?`, [page.cursor, SCOPE]);
        }
        for (const record of page.data) applyRecord(store, schema, record);
        after = page.next;
        store.exec(`UPDATE _hydration SET after_id = ?, done = ? WHERE object = ?`, [
          after,
          after === "" ? 1 : 0,
          row.object,
        ]);
      });

      if (after === "") break;
    }
  }

  return currentCursor(store);
}

/** hydrationStarted reports whether any object has taken a snapshot page yet.
 *
 * This is the predicate behind "keep the cursor from the *first* page": it is
 * true as soon as one object has advanced or finished, and it is derived from
 * the bookkeeping rather than stored separately so there is no second piece of
 * state to get out of step. */
function hydrationStarted(store: Store): boolean {
  const started = store.query<{ n: number }>(
    `SELECT COUNT(*) AS n FROM _hydration WHERE after_id <> '' OR done = 1`,
  );
  return Number(started[0].n) > 0;
}

/** pull drains the feed from the current cursor until it is caught up, and
 * returns the cursor it reached.
 *
 * A short page means caught up: the server returns up to `limit` entries and
 * fewer only when there are no more. An empty page leaves the cursor where it
 * was (the server echoes the caller's own cursor rather than zero, which would
 * rewind a caught-up client to the beginning). */
export async function pull(
  store: Store,
  index: SchemaIndex,
  transport: Transport,
): Promise<number> {
  if (!hydrated(store)) {
    throw new Error("sync: pull before hydration completed");
  }

  let cursor = currentCursor(store);
  for (;;) {
    const page = await transport.changes(cursor, PAGE_SIZE);
    if (page.data.length === 0) return cursor;

    cursor = applyPage(store, index, page);
    if (page.data.length < PAGE_SIZE) return cursor;
  }
}

/** sync is the whole reconnect cycle: generate the schema, send what the user
 * did offline, finish hydrating, then catch up. Safe to call repeatedly —
 * hydration is resumable, the feed is idempotent, and the drain is deduped by
 * command_id (INV-E4).
 *
 * **The order is drain → hydrate → pull** (WP-2.3-decisions.md §4), and the
 * only interesting part of it is drain before pull. Both orders are correct —
 * the server revalidates against current state regardless of what the client
 * knew (commandment 5) — so it is chosen on what the user sees. Pulling first
 * overwrites the optimistic rows with server state that predates the user's own
 * work, which makes their edits visibly disappear and then reappear a moment
 * later. Draining first means the server sees the work, the feed carries it
 * back, and the pull confirms it.
 *
 * Drain before hydrate matters only on a re-hydration after eviction, where
 * there is nothing to drain anyway. It is in that position on principle: the
 * outbox is the one thing in this replica hydration cannot reconstruct.
 *
 * WP-2.4's revocation purge goes **after** the drain, for the reason §4 records
 * — a purge that runs first deletes rows that queued commands reference. This
 * WP does not build the purge; it fixes the contract 2.4 has to honour. */
export async function sync(store: Store, transport: Transport): Promise<number> {
  const index = await open(store, transport);
  await drain(store, index, transport);
  await hydrate(store, index, transport);
  return pull(store, index, transport);
}

/** replicatedObjects lists what this replica holds, for a UI that wants to say
 * so. Exported because "every capability reachable via API" has a client-side
 * corollary: the replica should not be a black box to the shell hosting it. */
export function replicatedObjects(objects: MetaObject[]): string[] {
  return replicableObjects(objects).map((o) => o.name);
}

// SPDX-License-Identifier: AGPL-3.0-only

// The sync core: fold the server's change feed (WP-2.1) into a local replica,
// in order, exactly once, advancing a cursor that never runs ahead of what is
// committed.
//
// This is the WP-2.6 spike's subject. It is deliberately the smallest thing
// that exercises the real constraints — ordering, resume, crash-safety,
// idempotence — because the question being answered is "can one core in this
// language serve every shell", not "is the replica finished". WP-2.2 builds
// the real one; if this prototype's language wins, this file is its seed.

import type { SqlValue, Store } from "./port";

/** One entry of the server's change feed, as GET /api/v1/sync/changes returns. */
export interface Change {
  cursor: number;
  source: string;
  ref_id: string;
  object: string;
  scope_key: string;
  recorded_at: string;
}

/** The replica's own bookkeeping tables, plus a stand-in for replicated data.
 *
 * `_sync_state` is docs/04's cursor store. `replica_row` stands in for the
 * generated per-object tables WP-2.2 will emit from metadata — the apply loop
 * does not care which it writes, and generating them here would be building
 * WP-2.2 inside a spike. */
const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS _sync_state (
     scope TEXT PRIMARY KEY,
     cursor INTEGER NOT NULL
   )`,
  `CREATE TABLE IF NOT EXISTS replica_row (
     source TEXT NOT NULL,
     ref_id TEXT NOT NULL,
     object TEXT NOT NULL,
     recorded_at TEXT NOT NULL,
     PRIMARY KEY (source, ref_id)
   )`,
];

/** The single scope this prototype tracks. WP-2.4 makes scopes plural; the
 * cursor store is keyed by scope from the start so that change is additive. */
const SCOPE = "default";

export function initReplica(store: Store): void {
  store.transaction(() => {
    for (const stmt of SCHEMA) store.exec(stmt);
    store.exec(`INSERT OR IGNORE INTO _sync_state (scope, cursor) VALUES (?, 0)`, [SCOPE]);
  });
}

export function currentCursor(store: Store): number {
  const rows = store.query<{ cursor: number }>(
    `SELECT cursor FROM _sync_state WHERE scope = ?`,
    [SCOPE],
  );
  return rows.length > 0 ? Number(rows[0].cursor) : 0;
}

/**
 * applyBatch folds one page of the feed into the replica and returns the new
 * cursor.
 *
 * Three properties, all of them load-bearing and all of them tested:
 *
 *   - **Ordered.** Entries apply in cursor order. A page that arrives out of
 *     order is a bug upstream, so it is rejected rather than sorted into
 *     place — silently repairing it would hide the very defect INV-S5 exists
 *     to prevent.
 *   - **Exactly once.** Entries at or below the current cursor are skipped, so
 *     a client that crashes after committing but before acknowledging can
 *     safely re-request the same page.
 *   - **Crash-safe.** Rows and cursor move in one transaction. If this throws
 *     mid-page, the replica is exactly where it started.
 */
export function applyBatch(store: Store, changes: Change[]): number {
  return store.transaction(() => {
    // Two distinct comparisons, and collapsing them into one is a bug the
    // first version of this file had. `start` decides what has already been
    // applied — a client re-requesting from an older cursor legitimately
    // re-sends a prefix, and those entries are skipped. `previous` decides
    // whether the page itself ascends.
    //
    // With a single running cursor for both, a descending page ([3, 2]) is
    // indistinguishable from an overlapping one: 3 applies, then 2 looks
    // "already applied" and is silently dropped. The replica quietly loses an
    // entry the server sent — an INV-S3 divergence, detected nowhere.
    const start = currentCursor(store);
    let cursor = start;
    let previous = -1;

    for (const change of changes) {
      if (change.cursor <= previous) {
        throw new Error(
          `sync: feed page does not ascend: cursor ${change.cursor} after ${previous}`,
        );
      }
      previous = change.cursor;

      if (change.cursor <= start) continue; // already applied before this page
      cursor = change.cursor;
      upsert(store, change);
    }

    store.exec(`UPDATE _sync_state SET cursor = ? WHERE scope = ?`, [cursor, SCOPE]);
    return cursor;
  });
}

function upsert(store: Store, change: Change): void {
  const params: SqlValue[] = [change.source, change.ref_id, change.object, change.recorded_at];
  store.exec(
    `INSERT INTO replica_row (source, ref_id, object, recorded_at) VALUES (?, ?, ?, ?)
     ON CONFLICT (source, ref_id) DO UPDATE SET object = excluded.object, recorded_at = excluded.recorded_at`,
    params,
  );
}

/** rowCount is a convenience for tests and the bench. */
export function rowCount(store: Store): number {
  const rows = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM replica_row`);
  return Number(rows[0].n);
}

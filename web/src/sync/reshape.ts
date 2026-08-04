// SPDX-License-Identifier: AGPL-3.0-only

// The re-shape: docs/04 §Downstream 4, "the client performs partial re-shape
// (delete out-of-scope rows, fetch newly-in-scope)", and the revocation purge
// that goes with it.
//
// It is a diff, and it has no state of its own. `_hydration` already holds one
// row per object this replica replicates — that is what "I hold this object"
// means here — so the scope the server reports is compared against it directly
// rather than against a second record that could disagree with the first
// (WP-2.4-decisions.md §3).

import { tableName } from "./schema.ts";
import type { Store } from "./port.ts";

/** What one re-shape did, for a caller that wants to say so. */
export interface Reshape {
  /** Objects whose rows were purged: entitlement withdrawn, or the module
   * switched off. */
  purged: string[];
  /** Objects newly in scope, queued for the hydration that follows. */
  adopted: string[];
}

/**
 * reshape brings the replica's shape into line with `scope`.
 *
 * Two halves, and they run in the same pass because they are the same diff:
 *
 *   - **purge** (`held − scope`) deletes the object's rows and its `_hydration`
 *     row. The generated table itself stays: its schema comes from metadata
 *     (ADR-006), not from the scope, so dropping it would mean re-running DDL
 *     on the next grant to arrive at the empty table we already had.
 *   - **adopt** (`scope − held`) inserts a `_hydration` row, which is all a new
 *     entitlement needs — the hydrate() that follows pages it in.
 *
 * **It never issues a statement against `_outbox`, `_conflicts` or `_pending`,
 * and that is the point of the whole file.** A purge deletes the server's data;
 * the outbox holds the *user's*, which no re-fetch reconstructs. Losing a row
 * costs a fetch the user is no longer entitled to make; losing a command loses
 * work. The purge is not negotiable — a revocation a queued command could veto
 * would not be a revocation — so the two are separated rather than ordered
 * (decisions §5).
 *
 * Callers run it on **every** cycle, not only when the scope changed. It is a
 * diff over a handful of names, and running it unconditionally is what makes
 * the resurrection case self-heal: a command rejected on a later cycle rolls
 * its row back into the replica from the pre-image, which for a revoked object
 * would otherwise reinstate a row the user may no longer hold. The same cycle's
 * re-shape removes it again.
 */
export function reshape(store: Store, scope: string[]): Reshape {
  const wanted = new Set(scope);

  return store.transaction(() => {
    const held = store
      .query<{ object: string }>(`SELECT object FROM _hydration ORDER BY object`)
      .map((row) => String(row.object));

    const purged: string[] = [];
    for (const object of held) {
      if (wanted.has(object)) continue;
      purge(store, object);
      purged.push(object);
    }

    const adopted: string[] = [];
    for (const object of scope) {
      if (held.includes(object)) continue;
      store.exec(`INSERT OR IGNORE INTO _hydration (object) VALUES (?)`, [object]);
      adopted.push(object);
    }

    return { purged, adopted };
  });
}

/** purge empties one object out of the replica.
 *
 * The table may not exist: an object can leave `/meta/objects` (a module
 * disabled) in the same breath as it leaves the scope, and on a replica created
 * after that there was never a table to empty. That is a no-op, not a failure —
 * the desired end state is "no rows", and there are none. */
function purge(store: Store, object: string): void {
  const table = tableName(object);
  const exists = store.query<{ n: number }>(
    `SELECT COUNT(*) AS n FROM sqlite_master WHERE type = 'table' AND name = ?`,
    [table],
  );
  if (Number(exists[0].n) > 0) store.exec(`DELETE FROM ${table}`);
  store.exec(`DELETE FROM _hydration WHERE object = ?`, [object]);
}

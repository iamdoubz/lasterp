// SPDX-License-Identifier: AGPL-3.0-only

// Remote wipe, client side (WP-2.5, docs/08 §Data protection).
//
// The server cannot reach into this device; it can only refuse it and say why.
// Everything destructive happens here, on the strength of that refusal.

import { tableName, type MetaObject } from "./schema.ts";
import type { Store } from "./port.ts";

/** DeviceWipedError is the transport's translation of a 401 carrying
 * `type: "device-wiped"`.
 *
 * It is a distinct class because it must **short-circuit the whole sync cycle**
 * rather than be read as one request failing. `kernel/api/gateway.go` says it
 * plainly: 401 is the one status this product's clients act on destructively —
 * the drain files queued work as rejected rather than retrying. Filing conflict
 * rows for commands on a device that is about to be erased would be work spent
 * writing into a database being deleted (WP-2.5-decisions.md §6). */
export class DeviceWipedError extends Error {
  constructor() {
    super("this device was remotely wiped; its local data must be destroyed");
    this.name = "DeviceWipedError";
  }
}

/**
 * wipeReplica destroys everything this replica holds.
 *
 * **Including `_outbox` and `_conflicts`, and that is the deliberate opposite
 * of WP-2.4's scope purge.** A purge spares queued commands because they are
 * the user's own work and no re-fetch reconstructs them (reshape.ts). A wipe
 * takes them, because the device is presumed to be in the wrong hands and
 * losing unsent work is the *point* — it is data on a machine that should not
 * have data.
 *
 * Getting these two backwards in either direction is a serious bug: a purge
 * that ate the outbox loses a user's work, and a wipe that spared it leaves the
 * thief a copy. Both carry a comment naming the other.
 *
 * Idempotent, because it may not get to finish. A tab closing mid-wipe, or a
 * pool whose handles are held, must not leave the device believing it
 * succeeded — the server keeps refusing, so the next open is told again.
 */
export function wipeReplica(store: Store, objects: MetaObject[]): void {
  store.transaction(() => {
    for (const object of objects) {
      const table = tableName(object.name);
      if (tableExists(store, table)) store.exec(`DELETE FROM ${table}`);
    }
    // Bookkeeping last: while these rows exist the replica still describes
    // itself, so a wipe interrupted after the data but before the bookkeeping
    // resumes correctly rather than looking like a hydrated empty replica.
    for (const table of ["_outbox", "_conflicts", "_pending", "_hydration", "_meta"]) {
      if (tableExists(store, table)) store.exec(`DELETE FROM ${table}`);
    }
    store.exec(`UPDATE _sync_state SET cursor = 0`);
  });
}

/** wipedClean reports whether nothing of substance survives — the assertion a
 * test needs, and cheap enough to be worth exporting rather than reimplementing
 * per call site. */
export function wipedClean(store: Store, objects: MetaObject[]): boolean {
  const counts = [
    ...objects.map((o) => tableName(o.name)),
    "_outbox",
    "_conflicts",
    "_pending",
    "_hydration",
    "_meta",
  ];
  for (const table of counts) {
    if (!tableExists(store, table)) continue;
    const rows = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM ${table}`);
    if (Number(rows[0].n) > 0) return false;
  }
  return true;
}

function tableExists(store: Store, table: string): boolean {
  const rows = store.query<{ n: number }>(
    `SELECT COUNT(*) AS n FROM sqlite_master WHERE type = 'table' AND name = ?`,
    [table],
  );
  return Number(rows[0].n) > 0;
}

// SPDX-License-Identifier: AGPL-3.0-only

// SQLite-WASM over OPFS — the shell the product actually ships on until Tauri
// (WP-4.7).
//
// It uses the **SAH-pool VFS**, not the default `opfs` one. The default needs
// SharedArrayBuffer, which needs COOP/COEP response headers, which this product
// does not send and should not start sending casually: cross-origin isolation
// breaks any third-party iframe embed, and docs/05 §UI extension slots plus
// WP-3.6 are built on sandboxed iframes. SAH-pool needs neither header, so
// choosing it keeps a Phase-3 door open at no cost here.
//
// **Either VFS requires a dedicated worker.** Both rest on
// FileSystemFileHandle.createSyncAccessHandle(), which browsers do not expose
// on the main thread — calling the installer there fails with "Missing required
// OPFS APIs". See ADR-017 §Consequences and worker.ts.

import sqlite3InitModule, { type Database, type Sqlite3Static } from "@sqlite.org/sqlite-wasm";

import type { SqlValue, Store } from "../port.ts";

let sqlite3: Sqlite3Static | undefined;
let poolPromise: ReturnType<Sqlite3Static["installOpfsSAHPoolVfs"]> | undefined;

const POOL_NAME = "lasterp-replica";

/** REPLICA_FILES is what one replica actually opens: the database, plus a
 * rollback journal while a transaction is in flight.
 *
 * POOL_CAPACITY is that plus headroom. The SAH pool is not a filesystem — it
 * reserves a fixed set of sync access handles up front and hands them out, so
 * exceeding the count fails with SQLITE_CANTOPEN rather than growing (ADR-017
 * §Consequences left the number to this WP).
 *
 * The headroom is deliberately small. A large capacity is not free — each slot
 * is a pre-created OPFS file — and, worse, it would hide growth: if a future WP
 * gives the replica a second database (WP-2.4's per-scope files are the obvious
 * candidate), the pool should be re-derived rather than silently absorb it.
 * `poolFileCount` exists so a test asserts this rather than a comment claiming
 * it was picked deliberately. */
export const REPLICA_FILES = 2;
export const POOL_CAPACITY = REPLICA_FILES + 2;

/** ReplicaLockedError means another tab holds this replica.
 *
 * The SAH pool takes its access handles **exclusively**, so a second tab
 * opening the same replica cannot. An ERP user having two tabs open is
 * ordinary, and the honest reading of an unhandled failure here is a blank
 * screen — so this is a distinct type the shell renders as a state rather than
 * an error nobody modelled.
 *
 * Coordination (Web Locks + a shared worker proxying the replica) is WP-2.3's,
 * which is where it belongs: an outbox with two writers is a correctness
 * problem, not a usability one. See WP-2.2b-decisions.md §5. */
export class ReplicaLockedError extends Error {
  constructor(cause: unknown) {
    // The underlying message rides along. Telling a user to close another tab
    // when the real fault was something else is worse than saying nothing, and
    // this class is only reached by a path that cannot distinguish further.
    super(
      "sync: this replica is already open in another tab " +
        `(${cause instanceof Error ? cause.message : String(cause)})`,
    );
    this.name = "ReplicaLockedError";
    this.cause = cause;
  }
}

/** requestPersistence asks the browser not to evict the replica.
 *
 * Browsers evict OPFS under storage pressure without warning, and iOS evicts on
 * inactivity. For this WP the cost is bounded — the replica is read-only, so an
 * evicted replica is a re-hydration rather than lost work — and the caller is
 * told the outcome so the shell can say so.
 *
 * **WP-2.3 must revisit the denied path before it accepts a single offline
 * write.** Its `_outbox` holds commands the server has never seen, which is the
 * one thing in this client that no amount of re-fetching reconstructs
 * (WP-2.2b-decisions.md §7). */
export async function requestPersistence(): Promise<boolean> {
  if (typeof navigator === "undefined" || navigator.storage?.persist === undefined) return false;
  try {
    if (await navigator.storage.persisted()) return true;
    return await navigator.storage.persist();
  } catch {
    // A browser that refuses to answer is a browser that has not granted it.
    return false;
  }
}

async function acquirePool() {
  sqlite3 ??= await sqlite3InitModule();
  poolPromise ??= sqlite3.installOpfsSAHPoolVfs({
    name: POOL_NAME,
    initialCapacity: POOL_CAPACITY,
  });
  try {
    return await poolPromise;
  } catch (err) {
    // A failed install leaves a rejected promise cached, which would turn one
    // transient failure into a permanently broken replica for this page.
    poolPromise = undefined;
    throw new ReplicaLockedError(err);
  }
}

/** poolFileCount reports how many slots the pool currently has in use, so the
 * capacity above is asserted by a test rather than assumed. */
export async function poolFileCount(): Promise<number> {
  const pool = await acquirePool();
  return pool.getFileNames().length;
}

/** poolCapacity reports the configured slot count. */
export async function poolCapacity(): Promise<number> {
  const pool = await acquirePool();
  return pool.getCapacity();
}

/** wipePool returns the replica's storage to the browser (WP-2.5, INV-D1).
 *
 * `wipeReplica` in wipe.ts deletes rows; **that is not enough on its own.**
 * SQLite frees pages into the file's freelist rather than to the filesystem, so
 * after a DELETE the data is still sitting in the OPFS file, recoverable by
 * anyone who can read the origin's storage — which on a stolen device is
 * whoever has the machine. This is the call that actually reclaims it.
 *
 * The caller closes the database first: the pool holds its access handles
 * exclusively, and wiping underneath an open connection is how a half-erased
 * file gets left behind.
 *
 * Best-effort by necessity (decisions §7). A tab closing mid-wipe cannot be
 * made atomic from in here, so the local wipe is idempotent instead: the server
 * keeps refusing the device, so the next open is told again and tries again. */
export async function wipePool(): Promise<void> {
  const pool = await acquirePool();
  await pool.wipeFiles();
}

/** Opens (and on first call initialises) an OPFS-backed replica. */
export async function openOpfsStore(name = "lasterp-replica.sqlite3"): Promise<Store> {
  const pool = await acquirePool();
  // Deliberately not wrapped as ReplicaLockedError. Failing here is a *file*
  // failure — most likely SQLITE_CANTOPEN from exhausting the pool's fixed
  // slots — and reporting that as "open in another tab" would send someone to
  // close a tab that does not exist. Contention is detected at install time
  // (acquirePool), because that is where the exclusive handles are taken.
  const db: Database = new pool.OpfsSAHPoolDb(`/${name}`);
  let depth = 0;

  return {
    exec(sql: string, params: SqlValue[] = []): void {
      db.exec({ sql, bind: params });
    },

    query<T>(sql: string, params: SqlValue[] = []): T[] {
      return db.exec({ sql, bind: params, rowMode: "object", returnValue: "resultRows" }) as T[];
    },

    transaction<T>(fn: () => T): T {
      if (depth > 0) {
        depth++;
        try {
          return fn();
        } finally {
          depth--;
        }
      }
      db.exec("BEGIN");
      depth = 1;
      try {
        const out = fn();
        db.exec("COMMIT");
        return out;
      } catch (err) {
        db.exec("ROLLBACK");
        throw err;
      } finally {
        depth = 0;
      }
    },

    close(): void {
      db.close();
    },
  };
}

/** Deletes every replica file in the pool so a run starts clean. */
export async function resetOpfs(): Promise<void> {
  const pool = await acquirePool();
  pool.wipeFiles();
}

/** unlinkOpfs releases one file's slot back to the pool.
 *
 * The pool's slots are a fixed budget, not a filesystem: a file that is closed
 * still holds its slot until it is unlinked. Production opens one replica and
 * never needs this; the browser harness opens a database per case and would
 * otherwise exhaust a capacity sized for the real thing. */
export async function unlinkOpfs(name: string): Promise<void> {
  const pool = await acquirePool();
  pool.unlink(`/${name}`);
}

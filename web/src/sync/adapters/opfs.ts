// SPDX-License-Identifier: AGPL-3.0-only

// SQLite-WASM over OPFS — the browser half of WP-2.6 bar 1.
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
// OPFS APIs". The replica therefore lives in a worker regardless of the
// language the core is written in, which is a constraint on WP-2.2's shape and
// is recorded in ADR-017 §Consequences rather than left here.
//
// Both points are spike findings: a design that reached for the default VFS
// would have taxed the whole product with COOP/COEP, and one that assumed the
// main thread would have had to be rewritten at the first OPFS call.

import sqlite3InitModule, { type Database, type Sqlite3Static } from "@sqlite.org/sqlite-wasm";

import type { SqlValue, Store } from "../port";

let sqlite3: Sqlite3Static | undefined;
let poolPromise: ReturnType<Sqlite3Static["installOpfsSAHPoolVfs"]> | undefined;

/** POOL_CAPACITY is the number of database files the VFS pre-allocates slots
 * for. The SAH pool is not a filesystem — it reserves a fixed set of handles up
 * front and hands them out, so exceeding the count fails with SQLITE_CANTOPEN
 * rather than growing. The default is 6; WP-2.2 needs to pick this deliberately
 * once it knows how many files a replica actually uses. */
const POOL_CAPACITY = 16;

async function acquirePool() {
  sqlite3 ??= await sqlite3InitModule();
  poolPromise ??= sqlite3.installOpfsSAHPoolVfs({
    name: "lasterp-spike",
    initialCapacity: POOL_CAPACITY,
  });
  return poolPromise;
}

/** Opens (and on first call initialises) an OPFS-backed replica. */
export async function openOpfsStore(name = "lasterp-replica.sqlite3"): Promise<Store> {
  const pool = await acquirePool();
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

// SPDX-License-Identifier: AGPL-3.0-only

// node:sqlite adapter — the native-driver half of WP-2.6 bar 1, and the reason
// the core's properties can be tested headlessly (bar 4).
//
// It stands in for Tauri's native SQLite: a genuinely different driver with a
// different API from SQLite-WASM, which is what makes it a real test of the
// port rather than a rehearsal. Node 26 ships it, so this costs no dependency.

import { DatabaseSync } from "node:sqlite";

import type { SqlValue, Store } from "../port.ts";

export function openNodeStore(path = ":memory:"): Store {
  const db = new DatabaseSync(path);
  let depth = 0;

  return {
    exec(sql: string, params: SqlValue[] = []): void {
      db.prepare(sql).run(...params);
    },

    query<T>(sql: string, params: SqlValue[] = []): T[] {
      return db.prepare(sql).all(...params) as T[];
    },

    transaction<T>(fn: () => T): T {
      // Nesting is real: initReplica opens one and applyBatch opens another in
      // the same suite. SQLite has no nested transactions, so an inner call
      // joins the outer one rather than issuing a second BEGIN.
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

// SPDX-License-Identifier: AGPL-3.0-only

// ADR-017 bar 4: the core's properties are testable headlessly, with no browser
// in the loop — which is what lets WP-2.3's simulation harness run N virtual
// clients. Same suite as the browser run (src/sync/suite.ts); only the driver
// differs.

import { describe, expect, it } from "vitest";

import { openNodeStore } from "./adapters/node.ts";
import { applyPage, currentCursor, indexSchema, initReplica, rowCount } from "./core.ts";
import { benchPage, cases, SUITE_OBJECTS } from "./suite.ts";

describe("sync core (node:sqlite driver)", () => {
  for (const c of cases) {
    it(c.name, () => {
      const store = openNodeStore();
      try {
        c.run(store);
      } finally {
        store.close();
      }
    });
  }

  it("applies a large feed without the cursor and row count diverging", () => {
    const store = openNodeStore();
    try {
      initReplica(store, SUITE_OBJECTS);
      const n = 20_000;
      const cursor = applyPage(store, indexSchema(SUITE_OBJECTS), benchPage(n));
      expect(cursor).toBe(n);
      expect(currentCursor(store)).toBe(n);
      expect(rowCount(store, "Contact")).toBe(n);
    } finally {
      store.close();
    }
  });

  it("creates a table per replicable object and none for event-sourced ones", () => {
    const store = openNodeStore();
    try {
      initReplica(store, SUITE_OBJECTS);
      const tables = store
        .query<{ name: string }>(`SELECT name FROM sqlite_master WHERE type = 'table'`)
        .map((r) => r.name);
      expect(tables).toContain("obj_contact");
      expect(tables).toContain("_sync_state");
      expect(tables).toContain("_hydration");
      expect(tables).not.toContain("obj_journalentry");
    } finally {
      store.close();
    }
  });

  it("is idempotent across a reopen", () => {
    // Opening an existing replica is the same call as creating one. A second
    // initReplica that dropped or recreated tables would silently discard a
    // hydrated replica on every page load.
    const store = openNodeStore();
    try {
      initReplica(store, SUITE_OBJECTS);
      applyPage(store, indexSchema(SUITE_OBJECTS), benchPage(3));
      initReplica(store, SUITE_OBJECTS);
      expect(rowCount(store, "Contact")).toBe(3);
      expect(currentCursor(store)).toBe(3);
    } finally {
      store.close();
    }
  });
});

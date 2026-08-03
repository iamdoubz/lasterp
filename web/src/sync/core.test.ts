// SPDX-License-Identifier: AGPL-3.0-only

// WP-2.6 bar 4: the core's properties are testable headlessly, with no browser
// in the loop — which is what lets WP-2.3's simulation harness run N virtual
// clients. Same suite as the browser run (src/sync/suite.ts); only the driver
// differs.

import { describe, expect, it } from "vitest";

import { openNodeStore } from "./adapters/node";
import { applyBatch, currentCursor, initReplica, rowCount } from "./core";
import { benchFeed, cases } from "./suite";

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
      initReplica(store);
      const n = 20_000;
      const cursor = applyBatch(store, benchFeed(n));
      expect(cursor).toBe(n);
      expect(currentCursor(store)).toBe(n);
      expect(rowCount(store)).toBe(n);
    } finally {
      store.close();
    }
  });
});

// SPDX-License-Identifier: AGPL-3.0-only

// The conformance suite for the sync core, written once and run against every
// driver — WP-2.6 bar 1 ("one core, two drivers, no conditionals") is only
// demonstrated if the *same assertions* run on both.
//
// It is plain functions rather than vitest cases because one of its two hosts
// is a browser page with no test framework in it. vitest wraps each case in an
// it(); the browser runner calls them in a loop and reports.

import { applyBatch, currentCursor, initReplica, rowCount, type Change } from "./core";
import type { Store } from "./port";

export interface Case {
  name: string;
  run(store: Store): void;
}

function change(cursor: number, ref = `r${cursor}`): Change {
  return {
    cursor,
    source: "event",
    ref_id: ref,
    object: "Invoice",
    scope_key: "Invoice",
    recorded_at: "2026-08-02T00:00:00Z",
  };
}

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

export const cases: Case[] = [
  {
    // INV-S5's client half: what the server ordered is what the replica applies.
    name: "applies a page in order and advances the cursor",
    run(store) {
      initReplica(store);
      const cursor = applyBatch(store, [change(1), change(2), change(3)]);
      assert(cursor === 3, `cursor = ${cursor}, want 3`);
      assert(currentCursor(store) === 3, "cursor not persisted");
      assert(rowCount(store) === 3, `rows = ${rowCount(store)}, want 3`);
    },
  },
  {
    // docs/04: "Crash-safe: cursor advances only after commit."
    name: "a failed page leaves neither rows nor cursor behind",
    run(store) {
      initReplica(store);
      applyBatch(store, [change(1)]);

      let threw = false;
      try {
        // Descending mid-page. The core must refuse rather than sort it into
        // place or — worse — apply 3 and drop 2 as "already seen", which is
        // how a replica loses an entry the server actually sent.
        applyBatch(store, [change(3), change(2)]);
      } catch {
        threw = true;
      }
      assert(threw, "a descending page was accepted");
      assert(currentCursor(store) === 1, `cursor = ${currentCursor(store)}, want 1 after rollback`);
      assert(rowCount(store) === 1, `rows = ${rowCount(store)}, want 1 after rollback`);
    },
  },
  {
    // At-least-once delivery, exactly-once effect (docs/04 §Guarantees).
    name: "re-applying a page is a no-op",
    run(store) {
      initReplica(store);
      applyBatch(store, [change(1), change(2)]);
      const cursor = applyBatch(store, [change(1), change(2)]);
      assert(cursor === 2, `cursor = ${cursor}, want 2`);
      assert(rowCount(store) === 2, `rows = ${rowCount(store)}, want 2 — the page was applied twice`);
    },
  },
  {
    // Resume: paging from any position reconstructs the same replica.
    name: "resuming mid-feed converges to the same state as one pass",
    run(store) {
      initReplica(store);
      const feed = [1, 2, 3, 4, 5, 6].map((c) => change(c));
      applyBatch(store, feed.slice(0, 2));
      applyBatch(store, feed.slice(2, 4));
      applyBatch(store, feed.slice(4));
      assert(currentCursor(store) === 6, `cursor = ${currentCursor(store)}, want 6`);
      assert(rowCount(store) === 6, `rows = ${rowCount(store)}, want 6`);
    },
  },
  {
    // A page the client already passed is skipped, not replayed backwards —
    // the client re-requesting from an older cursor must not corrupt state.
    name: "entries at or below the cursor are skipped",
    run(store) {
      initReplica(store);
      applyBatch(store, [change(1), change(2), change(3)]);
      const cursor = applyBatch(store, [change(2), change(3), change(4)]);
      assert(cursor === 4, `cursor = ${cursor}, want 4`);
      assert(rowCount(store) === 4, `rows = ${rowCount(store)}, want 4`);
    },
  },
  {
    name: "an empty page leaves the cursor where it was",
    run(store) {
      initReplica(store);
      applyBatch(store, [change(1)]);
      const cursor = applyBatch(store, []);
      assert(cursor === 1, `cursor = ${cursor}, want 1`);
    },
  },
];

/** Synthetic feed page for the throughput bench. */
export function benchFeed(n: number, from = 0): Change[] {
  const out: Change[] = new Array(n);
  for (let i = 0; i < n; i++) out[i] = change(from + i + 1);
  return out;
}

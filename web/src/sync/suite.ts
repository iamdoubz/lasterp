// SPDX-License-Identifier: AGPL-3.0-only

// The conformance suite for the sync core, written once and run against every
// driver — ADR-017 bar 1 ("one core, two drivers, no conditionals") is only
// demonstrated if the *same assertions* run on both.
//
// It is plain functions rather than vitest cases because one of its two hosts
// is a browser page with no test framework in it. vitest wraps each case in an
// it(); the browser runner calls them in a loop and reports.

import { applyPage, currentCursor, indexSchema, initReplica, rowCount, type FeedPage } from "./core.ts";
import { ConformanceError, type MetaObject } from "./schema.ts";
import type { Store } from "./port.ts";

/** The schema the suite replicates. Small on purpose, but it carries one of
 * every shape the apply loop treats differently: a plain text column, an enum
 * with a declared option set (INV-T5), and a money field that must stay an
 * exact string. */
export const SUITE_OBJECTS: MetaObject[] = [
  {
    name: "Contact",
    resource: "/api/v1/contact",
    module: "contacts",
    persistence: "crud",
    fields: [
      { name: "name", type: "text", required: true },
      { name: "kind", type: "enum", required: true, options: ["customer", "supplier"] },
      { name: "credit_limit", type: "money", required: false },
    ],
  },
  {
    name: "JournalEntry",
    resource: "/api/v1/journalentry",
    module: "ledger",
    persistence: "event_sourced",
    fields: [{ name: "memo", type: "text", required: false }],
  },
];

const TENANT = "t-suite";

/** change builds one feed pointer. */
function change(cursor: number, ref = `r${cursor}`, object = "Contact") {
  return {
    cursor,
    source: "audit",
    ref_id: ref,
    object,
    scope_key: object,
    recorded_at: "2026-08-03T00:00:00Z",
  };
}

/** contact builds one materialised server record. */
function contact(id: string, name = `name-${id}`, extra: Record<string, unknown> = {}) {
  return {
    id,
    tenant_id: TENANT,
    created_at: "2026-08-03T00:00:00Z",
    updated_at: "2026-08-03T00:00:00Z",
    name,
    kind: "customer",
    credit_limit: null,
    ...extra,
  };
}

/** page builds a feed page whose pointers and rows agree, which is what the
 * server sends. Cases that need them to disagree build the page by hand. */
function page(cursors: number[], records = cursors.map((c) => contact(`r${c}`))): FeedPage {
  return {
    data: cursors.map((c) => change(c)),
    cursor: cursors.length > 0 ? cursors[cursors.length - 1] : 0,
    rows: records.length > 0 ? { Contact: records } : {},
  };
}

export interface Case {
  name: string;
  run(store: Store): void;
}

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

function setUp(store: Store) {
  initReplica(store, SUITE_OBJECTS);
  return indexSchema(SUITE_OBJECTS);
}

export const cases: Case[] = [
  {
    // INV-S5's client half: what the server ordered is what the replica applies.
    name: "applies a page in order and advances the cursor",
    run(store) {
      const index = setUp(store);
      const cursor = applyPage(store, index, page([1, 2, 3]));
      assert(cursor === 3, `cursor = ${cursor}, want 3`);
      assert(currentCursor(store) === 3, "cursor not persisted");
      assert(rowCount(store, "Contact") === 3, `rows = ${rowCount(store, "Contact")}, want 3`);
    },
  },
  {
    // docs/04: "Crash-safe: cursor advances only after commit."
    name: "a failed page leaves neither rows nor cursor behind",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1]));

      let threw = false;
      try {
        // Descending mid-page. The core must refuse rather than sort it into
        // place or — worse — apply 3 and drop 2 as "already seen", which is how
        // a replica loses an entry the server actually sent.
        applyPage(store, index, page([3, 2]));
      } catch {
        threw = true;
      }
      assert(threw, "a descending page was accepted");
      assert(currentCursor(store) === 1, `cursor = ${currentCursor(store)}, want 1 after rollback`);
      assert(rowCount(store, "Contact") === 1, "rows survived a rolled-back page");
    },
  },
  {
    // At-least-once delivery, exactly-once effect (docs/04 §Guarantees).
    name: "re-applying a page is a no-op",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2]));
      const cursor = applyPage(store, index, page([1, 2]));
      assert(cursor === 2, `cursor = ${cursor}, want 2`);
      assert(rowCount(store, "Contact") === 2, "the page was applied twice");
    },
  },
  {
    // Resume: paging from any position reconstructs the same replica.
    name: "resuming mid-feed converges to the same state as one pass",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2]));
      applyPage(store, index, page([3, 4]));
      applyPage(store, index, page([5, 6]));
      assert(currentCursor(store) === 6, `cursor = ${currentCursor(store)}, want 6`);
      assert(rowCount(store, "Contact") === 6, `rows = ${rowCount(store, "Contact")}, want 6`);
    },
  },
  {
    // A page the client already passed is skipped, not replayed backwards.
    name: "entries at or below the cursor are skipped",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2, 3]));
      const cursor = applyPage(store, index, page([2, 3, 4]));
      assert(cursor === 4, `cursor = ${cursor}, want 4`);
      assert(rowCount(store, "Contact") === 4, `rows = ${rowCount(store, "Contact")}, want 4`);
    },
  },
  {
    name: "an empty page leaves the cursor where it was",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1]));
      const cursor = applyPage(store, index, { data: [], cursor: 1, rows: {} });
      assert(cursor === 1, `cursor = ${cursor}, want 1`);
    },
  },
  {
    // Row state is *current* state, so a later value overwrites an earlier one
    // idempotently and last-writer-wins per row (WP-2.2-decisions.md §2).
    name: "a row that changed twice lands on its current value",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1], [contact("r1", "before")]));
      applyPage(store, index, page([2], [contact("r1", "after")]));
      const rows = store.query<{ name: string }>(`SELECT name FROM obj_contact WHERE id = 'r1'`);
      assert(rows.length === 1, `rows = ${rows.length}, want 1`);
      assert(rows[0].name === "after", `name = ${rows[0].name}, want "after"`);
    },
  },
  {
    // WP-2.2-decisions.md §5: a delete the replica is not told about is a
    // divergence no amount of syncing repairs — the one failure mode INV-S3
    // exists to catch. Archived rows arrive flagged and are removed locally.
    name: "an archived row is deleted from the replica",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2], [contact("r1"), contact("r2")]));
      assert(rowCount(store, "Contact") === 2, "setup did not land two rows");

      applyPage(
        store,
        index,
        page([3], [contact("r1", "name-r1", { archived_at: "2026-08-03T01:00:00Z" })]),
      );
      assert(rowCount(store, "Contact") === 1, "archived row was not removed");
      const left = store.query<{ id: string }>(`SELECT id FROM obj_contact`);
      assert(left[0].id === "r2", `surviving row = ${left[0].id}, want r2`);
    },
  },
  {
    // INV-T5 at the replica boundary: a value outside its declared option set
    // is surfaced, not coerced, and the whole page rolls back.
    name: "a non-conforming value rolls the page back instead of being stored",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1], [contact("r1")]));

      let caught: unknown;
      try {
        applyPage(store, index, page([2], [contact("r2", "bad", { kind: "banana" })]));
      } catch (err) {
        caught = err;
      }
      assert(caught instanceof ConformanceError, `want ConformanceError, got ${String(caught)}`);
      assert(currentCursor(store) === 1, `cursor = ${currentCursor(store)}, want 1`);
      assert(rowCount(store, "Contact") === 1, "a rolled-back page left a row behind");
    },
  },
  {
    // Money must stay an exact string. A number here means precision was lost
    // upstream, and storing it would put a wrong amount in the replica.
    name: "a numeric money value is refused",
    run(store) {
      const index = setUp(store);
      let threw = false;
      try {
        applyPage(store, index, page([1], [contact("r1", "n", { credit_limit: 1999 })]));
      } catch (err) {
        threw = err instanceof ConformanceError;
      }
      assert(threw, "a float money value was accepted into the replica");
      assert(rowCount(store, "Contact") === 0, "a refused row was stored anyway");
    },
  },
  {
    // Event-sourced pointers ride in `data` so the cursor stays honest, but
    // they resolve to no row: materialize() only joins audit-sourced entries.
    name: "an event-sourced pointer advances the cursor and stores no row",
    run(store) {
      const index = setUp(store);
      const cursor = applyPage(store, index, {
        data: [{ ...change(1, "e1", "JournalEntry"), source: "event" }],
        cursor: 1,
        rows: {},
      });
      assert(cursor === 1, `cursor = ${cursor}, want 1`);
      assert(rowCount(store, "Contact") === 0, "an event pointer wrote a contact");
    },
  },
  {
    // Rows for an object the replica has no table for would be dropped
    // silently. That is data the client was sent and did not keep — refuse.
    name: "rows for an unknown object are refused, not skipped",
    run(store) {
      const index = setUp(store);
      let threw = false;
      try {
        applyPage(store, index, {
          data: [change(1, "x1", "Mystery")],
          cursor: 1,
          rows: { Mystery: [contact("x1")] },
        });
      } catch {
        threw = true;
      }
      assert(threw, "rows for an unknown object were silently dropped");
    },
  },
];

/** Synthetic feed page for the throughput bench. */
export function benchPage(n: number, from = 0): FeedPage {
  const data = new Array(n);
  const rows = new Array(n);
  for (let i = 0; i < n; i++) {
    const c = from + i + 1;
    data[i] = change(c);
    rows[i] = contact(`r${c}`);
  }
  return { data, cursor: from + n, rows: { Contact: rows } };
}

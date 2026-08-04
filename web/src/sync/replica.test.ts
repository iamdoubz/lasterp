// SPDX-License-Identifier: AGPL-3.0-only

// Hydration properties, headless. The transport is a fake rather than a live
// server because these assert the *client's* paging and cursor bookkeeping;
// convergence against a real server is the Go-side property test that carries
// INV-S3 (internal/app/sync_converge_integrity_test.go).

import { beforeEach, describe, expect, it } from "vitest";

import { openNodeStore } from "./adapters/node.ts";
import { currentCursor, indexSchema, initReplica, rowCount, type FeedPage, type ServerRecord } from "./core.ts";
import { hydrate, hydrated, open, pull, sync } from "./replica.ts";
import { SUITE_OBJECTS } from "./suite.ts";
import type { CommandResult, SnapshotPage, Transport } from "./transport.ts";
import type { Store } from "./port.ts";

const TENANT = "t-hydrate";

function contact(id: string, name = `name-${id}`): ServerRecord {
  return {
    id,
    tenant_id: TENANT,
    created_at: "2026-08-03T00:00:00Z",
    updated_at: "2026-08-03T00:00:00Z",
    name,
    kind: "customer",
    credit_limit: null,
  };
}

/** fakeTransport pages a fixed row set, with a configurable page size so a
 * multi-page hydration is a few rows rather than a thousand. */
class FakeTransport implements Transport {
  rows: ServerRecord[] = [];
  /** cursor advances on every snapshot call, so a test can tell whether the
   * client kept the first page's value or a later one. */
  nextCursor = 100;
  pageSize = 2;
  feed: FeedPage[] = [];
  snapshotCalls = 0;
  /** failAfter makes the nth snapshot call throw, to prove resume. */
  failAfter = Infinity;

  async meta() {
    return SUITE_OBJECTS;
  }

  async snapshot(object: string, after: string): Promise<SnapshotPage> {
    this.snapshotCalls++;
    if (this.snapshotCalls > this.failAfter) throw new Error("network down");

    const all = object === "Contact" ? this.rows : [];
    const start = after === "" ? 0 : all.findIndex((r) => r["id"] === after) + 1;
    const slice = all.slice(start, start + this.pageSize);
    const last = slice[slice.length - 1];
    const next = start + this.pageSize < all.length && last ? String(last["id"]) : "";
    return { object, data: slice, cursor: this.nextCursor++, next };
  }

  async changes(): Promise<FeedPage> {
    return this.feed.shift() ?? { data: [], cursor: 0, rows: {} };
  }

  /** These tests are downstream-only, so a command reaching the wire means the
   * outbox leaked one — hence a failure rather than a stub. The drain's own
   * behaviour is outbox.test.ts. */
  async command(): Promise<CommandResult> {
    throw new Error("hydration tests should never replay a command");
  }
}

describe("hydration", () => {
  let store: Store;
  let transport: FakeTransport;

  beforeEach(() => {
    store = openNodeStore();
    transport = new FakeTransport();
  });

  it("pages every object's snapshot into the replica", async () => {
    transport.rows = ["a", "b", "c", "d", "e"].map((id) => contact(id));
    const index = await open(store, transport);
    await hydrate(store, index, transport);

    expect(rowCount(store, "Contact")).toBe(5);
    expect(hydrated(store)).toBe(true);
  });

  it("keeps the cursor from the first page, not a later one", async () => {
    // WP-2.2-decisions.md §4. Taking a later page's high-water mark would skip
    // every change committed between the two pages — the rows would be in the
    // snapshot for some objects and in neither for others.
    transport.rows = ["a", "b", "c", "d", "e"].map((id) => contact(id));
    transport.nextCursor = 100;

    const index = await open(store, transport);
    const cursor = await hydrate(store, index, transport);

    expect(transport.snapshotCalls).toBeGreaterThan(1);
    expect(cursor).toBe(100);
    expect(currentCursor(store)).toBe(100);
  });

  it("resumes an interrupted hydration instead of restarting it", async () => {
    transport.rows = ["a", "b", "c", "d", "e", "f"].map((id) => contact(id));
    transport.failAfter = 2;

    const index = await open(store, transport);
    await expect(hydrate(store, index, transport)).rejects.toThrow("network down");

    const landed = rowCount(store, "Contact");
    expect(landed).toBe(4);
    expect(hydrated(store)).toBe(false);

    transport.failAfter = Infinity;
    await hydrate(store, index, transport);

    expect(rowCount(store, "Contact")).toBe(6);
    expect(hydrated(store)).toBe(true);
    // The cursor is still the one recorded on the very first page.
    expect(currentCursor(store)).toBe(100);
  });

  it("refuses to pull before hydration completes", async () => {
    // Folding feed entries into a half-hydrated replica applies updates to rows
    // that are not there yet, and ON CONFLICT would manufacture partial rows
    // out of an update payload.
    transport.rows = ["a", "b", "c", "d"].map((id) => contact(id));
    initReplica(store, SUITE_OBJECTS);
    const index = indexSchema(SUITE_OBJECTS);

    expect(hydrated(store)).toBe(false);
    await expect(pull(store, index, transport)).rejects.toThrow(/before hydration/);
  });

  it("repairs a row that shifted mid-snapshot from the feed", async () => {
    // The reason paging a live table is safe: whatever the snapshot missed or
    // caught half-way is in the feed after the recorded cursor, and applying it
    // is idempotent.
    transport.rows = ["a", "b"].map((id) => contact(id, "stale"));
    transport.feed = [
      {
        data: [
          {
            cursor: 101,
            source: "audit",
            ref_id: "a",
            object: "Contact",
            scope_key: "Contact",
            recorded_at: "2026-08-03T00:00:01Z",
          },
        ],
        cursor: 101,
        rows: { Contact: [contact("a", "repaired")] },
      },
    ];

    await sync(store, transport);

    const names = store.query<{ name: string }>(`SELECT name FROM obj_contact ORDER BY id`);
    expect(names.map((r) => r.name)).toEqual(["repaired", "stale"]);
    expect(currentCursor(store)).toBe(101);
  });

  it("creates no hydration row for an event-sourced object", async () => {
    const index = await open(store, transport);
    await hydrate(store, index, transport);

    const tracked = store
      .query<{ object: string }>(`SELECT object FROM _hydration ORDER BY object`)
      .map((r) => r.object);
    expect(tracked).toEqual(["Contact"]);
  });
});

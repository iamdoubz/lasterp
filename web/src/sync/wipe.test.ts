// SPDX-License-Identifier: AGPL-3.0-only

// The wipe's mechanics, headless. The end-to-end criterion — an administrator
// wipes, the device honors it at its next connect — is the Go-side test that
// carries the AC (internal/app/device_integrity_test.go).

import { beforeEach, describe, expect, it } from "vitest";

import { openNodeStore } from "./adapters/node.ts";
import { applyRecord, indexSchema, initReplica, rowCount, type ServerRecord } from "./core.ts";
import { enqueue, outboxDepth, type Command } from "./outbox.ts";
import { reshape } from "./reshape.ts";
import { SUITE_OBJECTS } from "./suite.ts";
import { wipeReplica, wipedClean } from "./wipe.ts";
import type { Store } from "./port.ts";

const TENANT = "t-wipe";

function contact(id: string): ServerRecord {
  return {
    id,
    tenant_id: TENANT,
    created_at: "2026-08-04T00:00:00Z",
    updated_at: "2026-08-04T00:00:00Z",
    name: `name-${id}`,
    kind: "customer",
    credit_limit: null,
  };
}

function queued(id: string): Command {
  return {
    commandId: `018f0000-0000-7000-8000-${id.padStart(12, "0")}`,
    method: "POST",
    object: "Contact",
    rowId: `row-${id}`,
    body: { id: `row-${id}`, name: "unsent work", kind: "customer" },
  };
}

describe("wipe", () => {
  let store: Store;

  beforeEach(() => {
    store = openNodeStore();
    initReplica(store, SUITE_OBJECTS);
    const index = indexSchema(SUITE_OBJECTS);
    for (const id of ["a", "b", "c"]) applyRecord(store, index.get("Contact")!, contact(id));
    enqueue(store, index, queued("1"), true);
  });

  it("destroys replicated rows", () => {
    expect(rowCount(store, "Contact")).toBeGreaterThan(0);
    wipeReplica(store, SUITE_OBJECTS);
    expect(rowCount(store, "Contact")).toBe(0);
  });

  it("destroys unsent work, unlike a scope purge", () => {
    // WP-2.5-decisions.md §5. The device is presumed to be in the wrong hands,
    // so losing unsent work is the point — it is data on a machine that should
    // not have data. The very next test is the opposite rule, deliberately.
    expect(outboxDepth(store)).toBe(1);
    wipeReplica(store, SUITE_OBJECTS);
    expect(outboxDepth(store)).toBe(0);
    expect(wipedClean(store, SUITE_OBJECTS)).toBe(true);
  });

  it("a scope purge still spares unsent work", () => {
    // The paired regression. These two rules live in the same subsystem and say
    // opposite things; a change to either that leaks into the other is a
    // serious bug — a purge that ate the outbox loses a user's work, a wipe
    // that spared it leaves the thief a copy.
    reshape(store, []);
    expect(rowCount(store, "Contact")).toBe(0);
    expect(outboxDepth(store)).toBe(1);
  });

  it("clears the cached schema, so nothing describes the tenant afterwards", () => {
    wipeReplica(store, SUITE_OBJECTS);
    const meta = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM _meta`);
    expect(Number(meta[0].n)).toBe(0);
  });

  it("resets the cursor rather than leaving it ahead of an empty replica", () => {
    // A surviving cursor would make the next sync resume mid-feed into a
    // replica holding nothing — updates applied to rows that are not there.
    wipeReplica(store, SUITE_OBJECTS);
    const rows = store.query<{ cursor: number }>(`SELECT cursor FROM _sync_state`);
    for (const row of rows) expect(Number(row.cursor)).toBe(0);
  });

  it("is idempotent, because it may not get to finish", () => {
    wipeReplica(store, SUITE_OBJECTS);
    expect(() => wipeReplica(store, SUITE_OBJECTS)).not.toThrow();
    expect(wipedClean(store, SUITE_OBJECTS)).toBe(true);
  });

  it("survives an object whose table this replica never generated", () => {
    // A wiped device falls back to its cached schema, which can name an object
    // a later metadata change removed. The end state asked for is "no rows".
    expect(() =>
      wipeReplica(store, [...SUITE_OBJECTS, { ...SUITE_OBJECTS[0], name: "Vanished" }]),
    ).not.toThrow();
  });
});

// SPDX-License-Identifier: AGPL-3.0-only

// The re-shape's mechanics, headless. The entitlement-change *scenarios* — a
// live server, a grant withdrawn between two syncs — are the Go-side property
// tests that carry the AC (internal/app/sync_scope_integrity_test.go). What is
// here is the diff itself, and above all the one thing it must never do.

import { beforeEach, describe, expect, it } from "vitest";

import { openNodeStore } from "./adapters/node.ts";
import { applyRecord, indexSchema, initReplica, rowCount, type ServerRecord } from "./core.ts";
import { enqueue, outboxDepth, type Command } from "./outbox.ts";
import { reshape } from "./reshape.ts";
import { SUITE_OBJECTS } from "./suite.ts";
import type { Store } from "./port.ts";

const TENANT = "t-reshape";

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

function held(store: Store): string[] {
  return store
    .query<{ object: string }>(`SELECT object FROM _hydration ORDER BY object`)
    .map((r) => String(r.object));
}

describe("reshape", () => {
  let store: Store;

  beforeEach(() => {
    store = openNodeStore();
    initReplica(store, SUITE_OBJECTS);
    const index = indexSchema(SUITE_OBJECTS);
    for (const id of ["a", "b", "c"]) {
      applyRecord(store, index.get("Contact")!, contact(id));
    }
  });

  it("purges the rows of an object that left the scope", () => {
    expect(rowCount(store, "Contact")).toBe(3);

    const result = reshape(store, []);

    expect(result.purged).toEqual(["Contact"]);
    expect(rowCount(store, "Contact")).toBe(0);
    expect(held(store)).toEqual([]);
  });

  it("adopts an object that entered the scope, for hydration to fill", () => {
    reshape(store, []);
    expect(held(store)).toEqual([]);

    const result = reshape(store, ["Contact"]);

    expect(result.adopted).toEqual(["Contact"]);
    // Adopted, not filled: the re-shape queues the object and `hydrate` pages
    // it in. done = 0 is what makes that happen.
    expect(held(store)).toEqual(["Contact"]);
    expect(rowCount(store, "Contact")).toBe(0);
  });

  it("leaves an unchanged scope alone", () => {
    const result = reshape(store, ["Contact"]);

    expect(result).toEqual({ purged: [], adopted: [] });
    expect(rowCount(store, "Contact")).toBe(3);
  });

  it("never deletes queued work or a filed conflict", async () => {
    // The premortem's first WP-2.4 gate, and decisions §5: the purge takes the
    // server's data, never the user's. A command body is work nothing
    // reconstructs; a replica row is a copy of something the server still has.
    const index = indexSchema(SUITE_OBJECTS);
    const command: Command = {
      commandId: "018f0000-0000-7000-8000-000000000001",
      method: "POST",
      object: "Contact",
      rowId: "queued-row",
      body: { id: "queued-row", name: "unsent", kind: "customer" },
    };
    enqueue(store, index, command, true);
    expect(outboxDepth(store)).toBe(1);
    expect(rowCount(store, "Contact")).toBe(4); // three seeded + the optimistic one

    reshape(store, []);

    // The rows are gone — the revocation is not negotiable.
    expect(rowCount(store, "Contact")).toBe(0);
    // The command is not. It still carries its own body, which is what the
    // drain replays and what the tray renders.
    expect(outboxDepth(store)).toBe(1);
    const queued = store.query<{ body: string }>(`SELECT body FROM _outbox`);
    expect(String(queued[0].body)).toContain("unsent");
    // And the pending flag survives with it: it is keyed to the command, not
    // to the row, so a rollback later still knows what it is undoing.
    const pending = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM _pending`);
    expect(Number(pending[0].n)).toBe(1);
  });

  it("purges an object whose table this replica never generated", () => {
    // An object can leave `/meta/objects` (a module disabled) in the same
    // breath as it leaves the scope. The end state asked for is "no rows", and
    // on a replica created after that there are none — a no-op, not a failure.
    store.exec(`INSERT INTO _hydration (object) VALUES (?)`, ["Vanished"]);

    const result = reshape(store, ["Contact"]);

    expect(result.purged).toEqual(["Vanished"]);
    expect(held(store)).toEqual(["Contact"]);
  });
});

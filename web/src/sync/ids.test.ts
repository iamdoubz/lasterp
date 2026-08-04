// SPDX-License-Identifier: AGPL-3.0-only

// The server refuses anything that is not a canonical UUIDv7 (kernel/idgen
// IsV7), so this is proven end to end by the sync harness. It is also worth
// pinning here: when it broke, the symptom was a 422 three layers away, and
// `crypto.randomUUID()` looks close enough to right to be re-introduced by
// anyone who has not read why it is not.

import { describe, expect, it } from "vitest";

import { newId } from "./ids.ts";

const CANONICAL = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe("newId", () => {
  it("is a canonical UUIDv7", () => {
    for (let i = 0; i < 100; i++) {
      expect(newId()).toMatch(CANONICAL);
    }
  });

  it("encodes the current time in its leading 48 bits", () => {
    const before = Date.now();
    const id = newId();
    const after = Date.now();

    const ms = Number.parseInt(id.slice(0, 8) + id.slice(9, 13), 16);
    expect(ms).toBeGreaterThanOrEqual(before);
    expect(ms).toBeLessThanOrEqual(after);
  });

  it("sorts chronologically as a string", () => {
    // The whole reason ids are v7 (docs/03): lexicographic order is time order,
    // which is what keeps index locality good under insert load. A v4 would
    // pass every other assertion here and fail this one.
    const first = newId();
    const later = `${(Date.now() + 60_000).toString(16).padStart(12, "0").replace(/^(.{8})(.{4})$/, "$1-$2")}-7000-8000-000000000000`;
    expect([later, first].sort()).toEqual([first, later]);
  });

  it("does not repeat", () => {
    const ids = new Set(Array.from({ length: 1000 }, newId));
    expect(ids.size).toBe(1000);
  });
});

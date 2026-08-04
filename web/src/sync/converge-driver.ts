// SPDX-License-Identifier: AGPL-3.0-only

// The replica, driven from outside for the convergence property test.
//
// internal/app/sync_converge_integrity_test.go spawns this with Node 26 (which
// strips types natively, so there is no build step between the source under
// test and the thing being run), points it at a live server, and compares what
// it dumps to the server's own projection. That test carries INV-S3.
//
// Usage: node converge-driver.ts <base-url> <token> <db-path> [--drop-nth=N]
//
// The core is imported unmodified. Only the Transport differs from the browser:
// an absolute URL and a bearer token instead of a same-origin cookie, which is
// exactly why Transport is an interface.

import { openNodeStore } from "./adapters/node.ts";
import { sync } from "./replica.ts";
import { replicableObjects, tableName, type MetaObject } from "./schema.ts";
import type { FeedPage } from "./core.ts";
import type { SnapshotPage, Transport } from "./transport.ts";

const [baseURL, token, dbPath, ...flags] = process.argv.slice(2);

/** dropNth simulates a feed that skips entries: every Nth change (and its rows)
 * is discarded before the core ever sees it.
 *
 * This is WP-2.2b-decisions.md §8.1. A convergence test whose oracle is current
 * server state, driven by an apply loop that writes current server state, can
 * pass by construction — the final pass re-fetches over any earlier damage. If
 * the property still holds with entries deleted, it is not testing selection,
 * and the Go side asserts that this run **fails**. */
const dropNth = Number(
  flags.find((f) => f.startsWith("--drop-nth="))?.slice("--drop-nth=".length) ?? 0,
);

async function get<T>(path: string): Promise<T> {
  const response = await fetch(baseURL + path, {
    headers: { Accept: "application/json", Authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    throw new Error(`${path} -> ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as T;
}

let seen = 0;

const transport: Transport = {
  async meta(): Promise<MetaObject[]> {
    return (await get<{ data: MetaObject[] }>("/api/v1/meta/objects")).data;
  },

  async snapshot(object: string, after: string, limit: number): Promise<SnapshotPage> {
    const params = new URLSearchParams({ object, limit: String(limit) });
    if (after !== "") params.set("after", after);
    return get<SnapshotPage>(`/api/v1/sync/snapshot?${params}`);
  },

  async changes(after: number, limit: number): Promise<FeedPage> {
    const params = new URLSearchParams({
      after: String(after),
      limit: String(limit),
      include: "rows",
    });
    const page = await get<FeedPage>(`/api/v1/sync/changes?${params}`);
    return dropNth > 0 ? withDroppedEntries(page) : page;
  },
};

/** withDroppedEntries removes every Nth entry *and the row it names*, which is
 * what a feed that skipped a commit would look like to a client. Dropping the
 * pointer alone would leave the row in `rows` and the replica would still
 * converge — the mutation has to remove both to model the real failure. */
function withDroppedEntries(page: FeedPage): FeedPage {
  const kept: FeedPage["data"] = [];
  const dropped = new Set<string>();
  for (const change of page.data) {
    seen++;
    if (seen % dropNth === 0) {
      dropped.add(change.ref_id);
      continue;
    }
    kept.push(change);
  }

  const rows: NonNullable<FeedPage["rows"]> = {};
  for (const [object, records] of Object.entries(page.rows ?? {})) {
    rows[object] = records.filter((r) => !dropped.has(String(r["id"])));
  }
  return { data: kept, cursor: page.cursor, rows };
}

const store = openNodeStore(dbPath);
try {
  await sync(store, transport);

  // Dump every replica table, ordered by id, for the Go side to compare.
  const objects = replicableObjects(await transport.meta());
  const dump: Record<string, unknown[]> = {};
  for (const object of objects) {
    dump[object.name] = store.query(`SELECT * FROM ${tableName(object.name)} ORDER BY id`);
  }
  process.stdout.write(JSON.stringify(dump));
} finally {
  store.close();
}

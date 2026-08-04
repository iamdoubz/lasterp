// SPDX-License-Identifier: AGPL-3.0-only

// The browser half of WP-2.2b's acceptance criterion: the same core, on the
// shell that ships, over SQLite-WASM/OPFS **in a dedicated worker**.
//
// The worker is not a convenience. Both OPFS VFSes need
// FileSystemFileHandle.createSyncAccessHandle(), which browsers expose only in
// a dedicated worker — on the main thread the SAH-pool installer reports
// "Missing required OPFS APIs". ADR-017 §Consequences records that this would
// have bound a Rust core identically.
//
// What this proves that the Go convergence test cannot: the real VFS, the real
// slot pool, and a real second browsing context. What it deliberately does not
// re-prove: convergence against a live server across randomized operations and
// both dialects — that is internal/app/sync_converge_integrity_test.go, which
// carries INV-S3. The transport here is a stub serving a fixed dataset, so this
// spec measures the storage substrate rather than re-running the property.

import {
  openOpfsStore,
  poolCapacity,
  poolFileCount,
  resetOpfs,
  unlinkOpfs,
} from "../src/sync/adapters/opfs.ts";
import { applyPage, currentCursor, indexSchema, initReplica, rowCount } from "../src/sync/core.ts";
import type { FeedPage, ServerRecord } from "../src/sync/core.ts";
import { sync } from "../src/sync/replica.ts";
import { benchPage, cases, SUITE_OBJECTS } from "../src/sync/suite.ts";
import type { SnapshotPage, Transport } from "../src/sync/transport.ts";
import type { BrowserResults } from "./results.ts";

const BENCH_CHANGES = 50_000;
const BENCH_PAGE = 5_000;
const TENANT = "t-browser";

/** HYDRATE_ROWS spans more than one snapshot page so paging is exercised. */
const HYDRATE_ROWS = 5;
const STUB_PAGE = 2;

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

/** stubTransport serves a fixed dataset: a paged snapshot, then one feed page
 * that updates a row hydration already saw. */
function stubTransport(): Transport {
  const rows = Array.from({ length: HYDRATE_ROWS }, (_, i) => contact(`c${i}`));
  let feedDelivered = false;

  return {
    async meta() {
      return SUITE_OBJECTS;
    },
    async snapshot(object: string, after: string): Promise<SnapshotPage> {
      const all = object === "Contact" ? rows : [];
      const start = after === "" ? 0 : all.findIndex((r) => r["id"] === after) + 1;
      const slice = all.slice(start, start + STUB_PAGE);
      const last = slice[slice.length - 1];
      const next = start + STUB_PAGE < all.length && last ? String(last["id"]) : "";
      return { object, data: slice, cursor: 10, next };
    },
    async changes(): Promise<FeedPage> {
      if (feedDelivered) return { data: [], cursor: 11, rows: {} };
      feedDelivered = true;
      return {
        data: [
          {
            cursor: 11,
            source: "audit",
            ref_id: "c0",
            object: "Contact",
            scope_key: "Contact",
            recorded_at: "2026-08-03T00:00:01Z",
          },
        ],
        cursor: 11,
        rows: { Contact: [contact("c0", "updated-by-feed")] },
      };
    },
  };
}

async function run(): Promise<BrowserResults> {
  const results: BrowserResults = { suite: [] };

  // One database file per case. Relying on the SAH pool's wipeFiles() between
  // cases left state behind — the second case saw the first's cursor — and
  // fighting the VFS's reuse semantics is not what this proves.
  await resetOpfs();

  for (const [i, c] of cases.entries()) {
    const store = await openOpfsStore(`case-${i}.sqlite3`);
    try {
      c.run(store);
      results.suite.push({ name: c.name, ok: true });
    } catch (err) {
      results.suite.push({ name: c.name, ok: false, error: String(err) });
    } finally {
      store.close();
      // Give the slot back. The pool's capacity is sized for production — one
      // replica — and this harness opens a database per case, so without this
      // the *test* would exhaust a budget the product never comes near.
      await unlinkOpfs(`case-${i}.sqlite3`);
    }
  }

  // A full hydrate + apply cycle on the shell that ships.
  const replica = await openOpfsStore("converge.sqlite3");
  try {
    await sync(replica, stubTransport());
    results.converged = {
      rows: rowCount(replica, "Contact"),
      cursor: currentCursor(replica),
      names: replica
        .query<{ name: string }>(`SELECT name FROM obj_contact ORDER BY id`)
        .map((r) => r.name),
    };
  } finally {
    replica.close();
  }

  // Throughput, paged the way a hydrating client pages.
  const bench = await openOpfsStore("bench.sqlite3");
  try {
    initReplica(bench, SUITE_OBJECTS);
    const index = indexSchema(SUITE_OBJECTS);
    const started = performance.now();
    for (let from = 0; from < BENCH_CHANGES; from += BENCH_PAGE) {
      applyPage(bench, index, benchPage(BENCH_PAGE, from));
    }
    const ms = performance.now() - started;
    results.bench = {
      changes: BENCH_CHANGES,
      ms: Math.round(ms),
      perSecond: Math.round(BENCH_CHANGES / (ms / 1000)),
      rows: rowCount(bench, "Contact"),
    };
    if (currentCursor(bench) !== BENCH_CHANGES) {
      results.fatal = `cursor ${currentCursor(bench)} != ${BENCH_CHANGES}`;
    }
  } finally {
    bench.close();
  }

  // The pool's slot count against what a real replica actually opened. If this
  // ever exceeds capacity the failure is SQLITE_CANTOPEN at hydration time on a
  // large tenant, which is exactly the kind of thing that does not show up in a
  // two-table test — so it is measured rather than assumed.
  results.pool = { capacity: await poolCapacity(), filesInUse: await poolFileCount() };

  return results;
}

run()
  .then((r) => postMessage(r))
  .catch((err) => postMessage({ suite: [], fatal: String(err) } satisfies BrowserResults));

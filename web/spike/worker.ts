// SPDX-License-Identifier: AGPL-3.0-only

// The spike's browser run happens *in a dedicated worker*, and that is a
// finding rather than a convenience.
//
// SQLite-WASM's OPFS VFSes need FileSystemFileHandle.createSyncAccessHandle(),
// which browsers expose only in a dedicated worker — on the main thread the
// SAH-pool installer reports "Missing required OPFS APIs". So the replica, and
// therefore the sync core that drives it, lives in a worker no matter which
// language the core is written in. See ADR-017 §Consequences.

import { openOpfsStore, resetOpfs } from "../src/sync/adapters/opfs";
import { applyBatch, currentCursor, initReplica, rowCount } from "../src/sync/core";
import { benchFeed, cases } from "../src/sync/suite";
import type { SpikeResults } from "./results";

const BENCH_CHANGES = 50_000;
const BENCH_PAGE = 5_000;

async function run(): Promise<SpikeResults> {
  const results: SpikeResults = { suite: [] };

  // One database file per case. Relying on the SAH pool's wipeFiles() between
  // cases left state behind — the second case saw the first's cursor — and
  // fighting the VFS's reuse semantics is not what this spike is measuring.
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
    }
  }

  // Throughput, paged the way a hydrating client pages: one transaction per
  // page, against a real OPFS-backed file.
  const store = await openOpfsStore("bench.sqlite3");
  try {
    initReplica(store);
    const started = performance.now();
    for (let from = 0; from < BENCH_CHANGES; from += BENCH_PAGE) {
      applyBatch(store, benchFeed(BENCH_PAGE, from));
    }
    const ms = performance.now() - started;
    results.bench = {
      changes: BENCH_CHANGES,
      ms: Math.round(ms),
      perSecond: Math.round(BENCH_CHANGES / (ms / 1000)),
      rows: rowCount(store),
    };
    if (currentCursor(store) !== BENCH_CHANGES) {
      results.fatal = `cursor ${currentCursor(store)} != ${BENCH_CHANGES}`;
    }
  } finally {
    store.close();
  }

  return results;
}

run()
  .then((r) => postMessage(r))
  .catch((err) => postMessage({ suite: [], fatal: String(err) } satisfies SpikeResults));

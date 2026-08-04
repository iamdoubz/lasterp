// SPDX-License-Identifier: AGPL-3.0-only

import { expect, test } from "@playwright/test";

import type { BrowserResults } from "./results.ts";

// WP-2.2b's acceptance criterion, browser half: "one pass in a real browser
// over SQLite-WASM/OPFS in a worker, proving the same core converges on the
// shell that ships".
//
// The other half — convergence against a live server under randomized
// operations, on Postgres and SQLite — is
// internal/app/sync_converge_integrity_test.go, which carries INV-S3. Splitting
// them is deliberate: that test proves the property, this one proves the
// platform, and neither claims the other's ground.

test("the sync core hydrates, applies and converges on OPFS in a worker", async ({ page }) => {
  const failures: string[] = [];
  page.on("pageerror", (e) => failures.push(String(e)));

  await page.goto("/browser/index.html");
  await page.waitForFunction(() => window.__replica?.secondTab !== undefined, undefined, {
    timeout: 120_000,
  });

  const results = (await page.evaluate(() => window.__replica)) as BrowserResults;
  expect(results.fatal, `page reported: ${results.fatal}`).toBeUndefined();
  expect(failures, `uncaught page errors: ${failures.join("; ")}`).toHaveLength(0);

  // The shared suite: the same assertions the node:sqlite run makes. If both
  // pass, one core served two drivers with no platform branches in it.
  expect(results.suite.filter((c) => !c.ok).map((c) => `${c.name}: ${c.error}`)).toEqual([]);
  expect(results.suite.length).toBeGreaterThan(0);

  // A full hydrate + apply cycle: five rows paged two at a time, then one feed
  // entry updating a row the snapshot already delivered.
  const converged = results.converged!;
  expect(converged.rows).toBe(5);
  expect(converged.cursor).toBe(11);
  expect(converged.names[0]).toBe("updated-by-feed");

  // WP-2.2b-decisions.md §5: a competing context is refused with the distinct
  // error, not left with a blank screen.
  expect(results.secondTab!.refused, `second worker reported: ${results.secondTab!.error}`).toBe(
    true,
  );

  // WP-2.2b-decisions.md §6: the SAH pool's capacity, measured rather than
  // assumed. Exceeding it fails with SQLITE_CANTOPEN at hydration time on a
  // large tenant — a failure that does not show up in a small test unless the
  // count is asserted.
  const pool = results.pool!;
  console.log(`SAH pool: ${pool.filesInUse}/${pool.capacity} slots in use`);
  expect(pool.filesInUse).toBeLessThanOrEqual(pool.capacity);

  const bench = results.bench!;
  console.log(
    `OPFS apply: ${bench.changes} changes in ${bench.ms}ms = ${bench.perSecond}/s (${bench.rows} rows)`,
  );
  expect(bench.rows).toBe(bench.changes);
  expect(bench.perSecond).toBeGreaterThanOrEqual(5_000);
});

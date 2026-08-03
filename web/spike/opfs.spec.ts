// SPDX-License-Identifier: AGPL-3.0-only

import { expect, test } from "@playwright/test";

import type { SpikeResults } from "./results";

// WP-2.6 bars 1 and 2, measured in a real browser against a real OPFS file.
// The page runs the same suite the vitest run does; if these pass and those
// pass, one core served two drivers with no platform branches in it.

test("the sync core passes the same suite on SQLite-WASM/OPFS", async ({ page }) => {
  const failures: string[] = [];
  page.on("pageerror", (e) => failures.push(String(e)));

  await page.goto("/spike/index.html");
  await page.waitForFunction(() => window.__spike !== undefined, undefined, { timeout: 120_000 });

  const results = (await page.evaluate(() => window.__spike)) as SpikeResults;
  expect(results.fatal, `page reported: ${results.fatal}`).toBeUndefined();
  expect(failures, `uncaught page errors: ${failures.join("; ")}`).toHaveLength(0);

  const failed = results.suite.filter((c) => !c.ok);
  expect(failed.map((c) => `${c.name}: ${c.error}`)).toEqual([]);
  expect(results.suite.length).toBeGreaterThan(0);

  // Bar 2: >= 5,000 changes/sec applied into OPFS in batched transactions.
  // Reported unconditionally — the number is the spike's evidence whether it
  // passes or fails, and ADR-017 quotes it either way.
  const bench = results.bench!;
  console.log(
    `OPFS apply: ${bench.changes} changes in ${bench.ms}ms = ${bench.perSecond}/s (${bench.rows} rows)`,
  );
  expect(bench.rows).toBe(bench.changes);
  expect(bench.perSecond).toBeGreaterThanOrEqual(5_000);
});

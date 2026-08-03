// SPDX-License-Identifier: AGPL-3.0-only

// WP-2.6 spike harness, page side. All it does is start the worker that owns
// the replica and publish what comes back — the core itself never runs here,
// because OPFS's sync access handles do not exist on the main thread (see
// worker.ts).
//
// Spike scaffolding: served only by playwright.spike.config.ts, not reachable
// from the app.

import type { SpikeResults } from "./results";

const worker = new Worker(new URL("./worker.ts", import.meta.url), { type: "module" });

worker.onmessage = (e: MessageEvent<SpikeResults>) => {
  window.__spike = e.data;
  document.getElementById("out")!.textContent = JSON.stringify(e.data, null, 2);
};

worker.onerror = (e) => {
  window.__spike = { suite: [], fatal: e.message || "worker failed" };
  document.getElementById("out")!.textContent = String(e.message);
};

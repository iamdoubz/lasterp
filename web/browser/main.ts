// SPDX-License-Identifier: AGPL-3.0-only

// Browser harness, page side. It starts the worker that owns the replica,
// then — while that worker still holds the pool — starts a second one to prove
// a competing context is refused rather than left staring at a blank screen.
//
// The core never runs here: OPFS's sync access handles do not exist on the main
// thread. Harness scaffolding, served only by playwright.browser.config.ts and
// not reachable from the app.

import type { BrowserResults } from "./results.ts";

const out = document.getElementById("out")!;
const worker = new Worker(new URL("./worker.ts", import.meta.url), { type: "module" });

worker.onmessage = (e: MessageEvent<BrowserResults>) => {
  const results = e.data;

  // The first worker has finished its run but is still alive, so it still holds
  // the pool's access handles — which is the condition a second tab meets.
  const second = new Worker(new URL("./second-tab.ts", import.meta.url), { type: "module" });

  second.onmessage = (m: MessageEvent<{ refused: boolean; error: string }>) => {
    publish({ ...results, secondTab: m.data });
    second.terminate();
    worker.terminate();
  };

  second.onerror = (err) => {
    publish({ ...results, secondTab: { refused: false, error: err.message || "worker failed" } });
    second.terminate();
    worker.terminate();
  };
};

worker.onerror = (e) => {
  publish({ suite: [], fatal: e.message || "worker failed" });
};

function publish(results: BrowserResults): void {
  window.__replica = results;
  out.textContent = JSON.stringify(results, null, 2);
}

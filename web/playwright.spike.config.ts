// SPDX-License-Identifier: AGPL-3.0-only
import { defineConfig, devices } from "@playwright/test";

// WP-2.6 spike only. Separate from playwright.config.ts on purpose: the product
// e2e suite drives a compiled binary serving the built bundle, which is the
// right shape for testing what ships and the wrong shape for measuring a
// prototype that is not wired into the product. This one serves the spike page
// straight off Vite.
//
// If ADR-017 chooses TypeScript this config is deleted and the suite folds into
// WP-2.2's tests; if it chooses Rust, the whole prototype goes.

const PORT = process.env.LASTERP_SPIKE_PORT ?? "5199";

export default defineConfig({
  testDir: "./spike",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `pnpm vite --port ${PORT} --strictPort`,
    url: `http://localhost:${PORT}/spike/index.html`,
    reuseExistingServer: true,
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});

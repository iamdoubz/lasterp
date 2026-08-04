// SPDX-License-Identifier: AGPL-3.0-only
import { defineConfig, devices } from "@playwright/test";

// The browser half of WP-2.2b's AC, served straight off Vite.
//
// Separate from playwright.config.ts on purpose. The product e2e suite drives a
// compiled binary serving the built bundle, which is the right shape for
// testing what ships — and the wrong shape here, because the harness page
// (web/browser/) is scaffolding that must never enter the product bundle.
//
// This replaces playwright.spike.config.ts, which said it would be folded into
// WP-2.2's tests once ADR-017 chose a language. It did, and this is that.

const PORT = process.env.LASTERP_BROWSER_PORT ?? "5199";

export default defineConfig({
  testDir: "./browser",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `pnpm vite --port ${PORT} --strictPort`,
    url: `http://localhost:${PORT}/browser/index.html`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});

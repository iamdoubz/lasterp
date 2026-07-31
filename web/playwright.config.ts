// SPDX-License-Identifier: AGPL-3.0-only
import { defineConfig, devices } from "@playwright/test";

// The e2e suite drives the real product: a compiled `lasterp` binary serving
// the built web bundle from one origin, against a fresh SQLite database. No
// mocks and no dev server — the thing under test is what ships.
//
// The server is started by scripts/e2e-server.sh, which builds the binary,
// bootstraps a tenant, and serves web/dist. Playwright waits on /healthz.

const PORT = process.env.LASTERP_E2E_PORT ?? "8099";
const baseURL = `http://localhost:${PORT}`;

export default defineConfig({
  testDir: "./e2e",
  // The invoice lifecycle writes to one shared database, so the specs are
  // ordered and serial rather than racing each other through the ledger.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL,
    trace: "on-first-retry",
    // Secure cookies are accepted on http://localhost (browsers treat it as a
    // secure context), so the production cookie flags need no test-only
    // weakening — see WP-1.5-decisions.md §5.
    ignoreHTTPSErrors: false,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "bash ../scripts/e2e-server.sh",
    url: `${baseURL}/healthz`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});

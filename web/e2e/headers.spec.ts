// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test } from "@playwright/test";

// WP-1.10 AC: header assertions in the e2e suite.
//
// docs/08 has listed CSP and HSTS under "standard web hardening" since Phase 0
// and the product set no security header at all until now (phase-1-review.md P1
// item 3). These run against the real binary serving the real bundle, because
// the thing worth proving is not that a middleware exists but that nothing
// downstream of it — the static file server, the gateway, a document handler —
// drops the headers on the way out.

/** The headers every response must carry, and what they must say. */
const REQUIRED: Record<string, RegExp> = {
  "content-security-policy": /default-src 'self'/,
  "x-content-type-options": /^nosniff$/,
  "strict-transport-security": /max-age=\d+/,
  "x-frame-options": /^DENY$/i,
  "referrer-policy": /^no-referrer$/i,
};

/** Paths covering each way bytes leave this server: the SPA shell, a hashed
 * asset from the static file server, an API read, and an API error. */
const PATHS = ["/", "/healthz", "/api/v1/openapi.json", "/api/v1/meta/objects"];

for (const path of PATHS) {
  test(`security headers are present on ${path}`, async ({ request }) => {
    const response = await request.get(path);
    const headers = response.headers();

    for (const [name, pattern] of Object.entries(REQUIRED)) {
      expect(headers[name], `${path} is missing ${name}`).toBeDefined();
      expect(headers[name], `${path} has an unexpected ${name}`).toMatch(pattern);
    }
  });
}

// The CSP must be strict enough to be worth having. A policy that allows
// 'unsafe-inline' or 'unsafe-eval' is the failure mode this test exists to
// catch, because that is what a policy decays into the first time someone hits a
// wall with it.
test("the content security policy has no unsafe escapes", async ({ request }) => {
  const csp = (await request.get("/")).headers()["content-security-policy"];

  expect(csp).not.toContain("unsafe-inline");
  expect(csp).not.toContain("unsafe-eval");
  expect(csp).toContain("object-src 'none'");
  expect(csp).toContain("frame-ancestors 'none'");
  expect(csp).toContain("base-uri 'none'");
});

// The bundle must actually load under that policy. A strict CSP that the app
// violates is worse than none: the browser blocks the script, the page is blank,
// and the next person "fixes" it by adding 'unsafe-inline'.
test("the app loads with no CSP violations", async ({ page }) => {
  const violations: string[] = [];
  page.on("console", (msg) => {
    const text = msg.text();
    if (/content security policy|refused to (load|execute|apply)/i.test(text)) {
      violations.push(text);
    }
  });

  await page.goto("/");
  // The sign-in form rendering at all means React booted, which means the module
  // script and the stylesheet both passed the policy.
  await expect(page.getByLabel(/Tenant/)).toBeVisible();

  expect(violations, `CSP blocked something the app needs:\n${violations.join("\n")}`).toEqual([]);
});

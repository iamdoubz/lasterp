// SPDX-License-Identifier: AGPL-3.0-only
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// WP-1.8 AC: "fresh tenant shows live role dashboard from seed data". The server
// this runs against is bootstrapped and then seeded with the demo book by
// scripts/e2e-server.sh, exactly as an operator would — so the figures below
// come from posted journal entries, not fixtures.

const TENANT = process.env.LASTERP_E2E_TENANT ?? "acme";
const EMAIL = process.env.LASTERP_E2E_EMAIL ?? "admin@example.com";
const PASSWORD = process.env.LASTERP_E2E_PASSWORD ?? "e2e-p4ssw0rd";

async function scan(page: Page, label: string) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  const blocking = results.violations.filter(
    (v) => v.impact === "serious" || v.impact === "critical",
  );
  expect(
    blocking,
    `${label}: ${blocking.map((v) => `${v.id} (${v.impact}) — ${v.help}`).join("; ")}`,
  ).toEqual([]);
}

async function signIn(page: Page, locale = "en") {
  await page.goto(`/?locale=${locale}`);
  await page.getByLabel(locale === "de" ? /Mandant/ : /Tenant/).fill(TENANT);
  await page.getByLabel(locale === "de" ? /E-Mail/ : /Email/).fill(EMAIL);
  await page.getByLabel(locale === "de" ? /Passwort/ : /Passwort|Password/).fill(PASSWORD);
  await page.getByRole("button", { name: locale === "de" ? "Anmelden" : "Sign in" }).click();
  await expect(
    page.getByRole("navigation", { name: locale === "de" ? "Hauptnavigation" : "Main" }),
  ).toBeVisible();
}

test.describe.configure({ mode: "serial" });

test("signing in lands on a live role dashboard", async ({ page }) => {
  await signIn(page);

  // docs/21 §4: "New user logs in → their role's dashboard is simply there."
  // Asserted by name: an administrator lands on the executive overview, not on
  // whichever pack happens to sort first.
  await expect(page.getByRole("heading", { level: 1, name: "Executive overview" })).toBeVisible();

  const headline = page.getByRole("heading", { level: 2, name: "Revenue" });
  await expect(headline).toBeVisible();

  // The number is real: the seeded book posts consulting and licence revenue
  // into the current period, so the headline is neither empty nor zero.
  const card = page.locator("article", { has: headline });
  await expect(card).toContainText(/[1-9]/);
  await expect(card).toContainText("€");

  // …and it carries the comparison that makes it mean something, naming the
  // period it is measured against rather than floating free.
  await expect(card).toContainText(/vs \d{4}-\d{2}/);
  await expect(card).toContainText(/[+−-]?\d/);

  // The period the figures cover is stated, not implied.
  await expect(page.getByText(/Period \d{4}-\d{2}/)).toBeVisible();

  await scan(page, "dashboard");
});

test("supporting tiles render beneath the headline", async ({ page }) => {
  await signIn(page);

  for (const label of ["Cash position", "Net income", "Accounts receivable outstanding"]) {
    await expect(page.getByRole("heading", { level: 2, name: label })).toBeVisible();
  }

  // Every tile states a comparison or says explicitly that it has none.
  const tiles = page.locator("article");
  const count = await tiles.count();
  expect(count).toBeGreaterThan(3);
  for (let i = 0; i < count; i++) {
    await expect(tiles.nth(i)).toContainText(/vs \d{4}-\d{2}|No earlier period/);
  }
});

test("the dashboard is reachable by name and listed in the nav", async ({ page }) => {
  await signIn(page);

  const nav = page.getByRole("navigation", { name: "Main" });
  await expect(nav.getByRole("link", { name: "Dashboard" })).toBeVisible();

  await page.goto("/dashboards/ar");
  await expect(page.getByRole("heading", { level: 1, name: "Accounts receivable" })).toBeVisible();
  // The AR pack leads with overdue — what needs a phone call today.
  await expect(page.getByRole("heading", { level: 2, name: "Accounts receivable overdue" })).toBeVisible();
  await scan(page, "ar dashboard");
});

test("the dashboard is localized", async ({ page }) => {
  await signIn(page, "de");

  await expect(page.getByRole("heading", { level: 1, name: "Geschäftsführung" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Umsatz" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Liquidität" })).toBeVisible();
  await expect(page.getByText(/Periode \d{4}-\d{2}/)).toBeVisible();

  // German number formatting reaches the tiles, not just the labels.
  const card = page.locator("article", { has: page.getByRole("heading", { name: "Umsatz" }) });
  await expect(card).toContainText(/\d{1,3}(\.\d{3})*,\d{2}/);

  await scan(page, "dashboard (de)");
});

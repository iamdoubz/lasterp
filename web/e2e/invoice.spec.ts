// SPDX-License-Identifier: AGPL-3.0-only
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// WP-1.5 AC: the invoice lifecycle (draft → post → GL → PDF) driven through the
// real UI, plus an axe-core scan on every screen the flow visits (docs/17:
// "axe-core in CI from WP-1.5 onward").
//
// The flow deliberately mixes metadata-rendered screens (Account, Contact —
// generic CRUD) with the hand-built invoice screen, because that split is the
// design: financial documents are not generic CRUD (INV-F2/F5/F6).

const TENANT = process.env.LASTERP_E2E_TENANT ?? "acme";
const EMAIL = process.env.LASTERP_E2E_EMAIL ?? "admin@example.com";
const PASSWORD = process.env.LASTERP_E2E_PASSWORD ?? "e2e-p4ssw0rd";

/** scan fails the test on any serious or critical accessibility violation.
 * Moderate/minor are reported but not gating, so the bar rises deliberately
 * rather than by accident. */
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

async function signIn(page: Page) {
  await page.goto("/");
  await page.getByLabel(/Tenant/).fill(TENANT);
  await page.getByLabel(/Email/).fill(EMAIL);
  await page.getByLabel(/Password/).fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

/**
 * A per-run suffix for anything the server enforces uniqueness on (account
 * codes, emails, period codes). The suite must not assume an empty database:
 * `reuseExistingServer` is on locally, and a developer re-running it should not
 * have to remember to wipe state first.
 */
const RUN = Date.now().toString().slice(-6);

/** createRecord fills a metadata-rendered form and returns the new record's id
 * from the URL the app navigates to. */
async function createRecord(page: Page, resource: string, values: Record<string, string>) {
  await page.goto(`/o/${resource}/new`);
  for (const [field, value] of Object.entries(values)) {
    await page.locator(`#field-${field}`).fill(value);
  }

  // Wait on the create response rather than only on the URL: a refused save
  // leaves the browser on /new, and a naive `/o/<resource>/[^/]+$` pattern
  // matches that immediately — silently yielding the literal id "new".
  const [response] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().endsWith(`/api/v1/${resource}`) && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Save" }).click(),
  ]);
  expect(response.status(), await response.text()).toBe(201);

  await page.waitForURL(new RegExp(`/o/${resource}/(?!new$)[^/]+$`));
  const id = page.url().split("/").pop()!;
  expect(id, "create did not navigate to the new record").not.toBe("new");
  return id;
}

test.describe.configure({ mode: "serial" });

test("sign-in screen is accessible and rejects bad credentials", async ({ page }) => {
  await page.goto("/");
  await scan(page, "login");

  await page.getByLabel(/Tenant/).fill(TENANT);
  await page.getByLabel(/Email/).fill(EMAIL);
  await page.getByLabel(/Password/).fill("definitely-wrong");
  await page.getByRole("button", { name: "Sign in" }).click();

  await expect(page.getByRole("alert")).toBeVisible();
  // Still on the login screen — no session was issued.
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Sign in");
});

test("the session cookie is not reachable from JavaScript", async ({ page, context }) => {
  await signIn(page);

  // The credential must exist in the jar…
  const cookies = await context.cookies();
  const session = cookies.find((c) => c.name === "lasterp_session");
  expect(session, "no session cookie was set").toBeTruthy();
  expect(session!.httpOnly, "session cookie is readable by JS").toBe(true);
  expect(session!.sameSite).toBe("Strict");

  // …and be invisible to page scripts (WP-1.5-decisions.md §5).
  const visible = await page.evaluate(() => document.cookie);
  expect(visible).not.toContain("lasterp_session");
});

test("navigation is rendered from tenant metadata", async ({ page }) => {
  await signIn(page);
  const nav = page.getByRole("navigation", { name: "Main" });
  // Objects come from /api/v1/meta/objects, not a hardcoded menu.
  await expect(nav.getByRole("link", { name: "Contact" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Account" })).toBeVisible();
  await scan(page, "shell");
});

test("invoice lifecycle: draft → post → GL entry → PDF", async ({ page }) => {
  await signIn(page);
  // page.request shares the browser context's cookie jar, so these calls carry
  // the same HttpOnly session the UI is using. The standalone `request` fixture
  // has its own jar and would be unauthenticated.
  const request = page.request;

  // --- ledger accounts and a customer, through metadata-rendered forms ---
  const arAccount = await createRecord(page, "account", {
    code: `11${RUN}`,
    name: "Accounts receivable",
    type: "asset",
  });
  await scan(page, "account detail");

  const revenueAccount = await createRecord(page, "account", {
    code: `40${RUN}`,
    name: "Revenue",
    // "income", not "revenue": the ledger's closed account-type set is
    // {asset, liability, equity, income, expense}, and P&L/balance-sheet
    // classification keys off it. Generic CRUD does not validate the enum yet
    // (WP-1.6-decisions.md §5), so an out-of-set value is accepted here and
    // then silently missing from the financial statements.
    type: "income",
  });
  const taxAccount = await createRecord(page, "account", {
    code: `22${RUN}`,
    name: "Tax payable",
    type: "liability",
  });

  await page.goto("/o/contact");
  await scan(page, "contact list");
  const contact = await createRecord(page, "contact", {
    name: "Wile E. Coyote",
    email: `wile-${RUN}@acme.test`,
    kind: "customer",
  });

  // --- period, tax rate and the draft invoice go through their own routes ---
  // These are lifecycle/reference-data endpoints, not CRUD, so the test drives
  // them over the API with the session cookie the browser already holds.
  const api = async (path: string, body: unknown) => {
    const response = await request.post(path, {
      data: body,
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
    expect(response.ok(), `${path} → ${response.status()} ${await response.text()}`).toBeTruthy();
    return response.json();
  };

  // A period code unique to this run, so a re-run against the same database
  // does not collide with the period the previous run opened.
  const period = `2026-07-${RUN}`;
  await api("/api/v1/periods", {
    code: period,
    start_date: "2026-07-01",
    end_date: "2026-07-31",
  });
  await api("/api/v1/taxrates", {
    jurisdiction: "DE",
    category: "standard",
    rate: "0.19",
    as_of: "2020-01-01",
    name: "USt",
    provider: "e2e",
  });

  const invoice = await api("/api/v1/invoices", {
    contact_id: contact,
    currency: "EUR",
    issue_date: "2026-07-15",
    ar_account: arAccount,
    tax_account: taxAccount,
    lines: [
      {
        description: "Rocket-powered roller skates",
        quantity: 2,
        unit_price_minor: 50000,
        revenue_account: revenueAccount,
        tax_jurisdiction: "DE",
        tax_category: "standard",
      },
    ],
  });

  // --- post it through the UI ---
  await page.goto(`/invoices/${invoice.ID}`);
  await expect(page.getByTestId("invoice-status")).toHaveText("draft");
  // A draft carries no number: gapless sequences are allocated at acceptance
  // (INV-F6), not at creation.
  await expect(page.getByTestId("invoice-number")).toHaveText("—");
  await scan(page, "invoice draft");

  await page.getByLabel(/Period/).fill(period);

  // Wait on the posting response itself, so a refusal fails with the server's
  // problem+json detail instead of a blind timeout on the status text.
  const [postResponse] = await Promise.all([
    page.waitForResponse((r) => r.url().includes("/post") && r.request().method() === "POST"),
    page.getByTestId("post-invoice").click(),
  ]);
  expect(postResponse.status(), await postResponse.text()).toBe(200);

  await expect(page.getByTestId("invoice-status")).toHaveText("posted");
  await expect(page.getByTestId("invoice-number")).not.toHaveText("—");
  await scan(page, "invoice posted");

  // --- the GL entry it created balances (INV-F1) ---
  const entryId = await page.getByTestId("invoice-entry").textContent();
  expect(entryId).toBeTruthy();
  const entryResponse = await request.get(`/api/v1/journalentries/${entryId!.trim()}`);
  expect(entryResponse.ok()).toBeTruthy();
  const entry = await entryResponse.json();

  // ledger.Entry/Line carry no json tags, so the wire shape is PascalCase.
  let debits = 0;
  let credits = 0;
  for (const line of entry.Lines ?? []) {
    debits += line.Debit ?? 0;
    credits += line.Credit ?? 0;
  }
  expect(debits, "journal entry does not balance (INV-F1)").toBe(credits);
  expect(debits).toBeGreaterThan(0);

  // --- and the PDF renders ---
  const pdf = await request.get(`/api/v1/invoices/${invoice.ID}/pdf`);
  expect(pdf.ok()).toBeTruthy();
  expect(pdf.headers()["content-type"]).toContain("application/pdf");
  expect((await pdf.body()).subarray(0, 4).toString()).toBe("%PDF");
});

test("posted invoices cannot be edited back into a draft", async ({ page }) => {
  await signIn(page);
  // Invoice is absent from the metadata surface precisely because it must not
  // get a generic edit form (WP-1.4b-decisions.md §2) — assert the UI reflects
  // that rather than trusting the server alone.
  const nav = page.getByRole("navigation", { name: "Main" });
  await expect(nav.getByRole("link", { name: "Invoice" })).toHaveCount(0);
});

test("signing out revokes the session", async ({ page }) => {
  await signIn(page);
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Sign in");

  // A reload must not resurrect the session from a stale cookie.
  await page.reload();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Sign in");
});

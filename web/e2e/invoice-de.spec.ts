// SPDX-License-Identifier: AGPL-3.0-only
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// WP-1.7 AC: "invoice e2e fully localized incl. PDF in the first non-English
// locale". This is the WP-1.5 lifecycle run again in German — sign-in, the
// metadata-rendered screens (with translated object and field labels), the
// hand-built invoice screen, and the rendered PDF — plus the axe scan every
// screen gets from WP-1.5 onward (docs/17).

const TENANT = process.env.LASTERP_E2E_TENANT ?? "acme";
const EMAIL = process.env.LASTERP_E2E_EMAIL ?? "admin@example.com";
const PASSWORD = process.env.LASTERP_E2E_PASSWORD ?? "e2e-p4ssw0rd";

const RUN = Date.now().toString().slice(-6);

/** German is chosen with ?locale=de on the first load; the provider remembers
 * it, so subsequent navigations stay German without repeating the query. */
const DE = "?locale=de";

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

async function signInGerman(page: Page) {
  await page.goto(`/${DE}`);
  // The login screen is localized before any session exists — which is why the
  // packs ship with the bundle rather than being fetched from an API that
  // would have to be public to be reachable here.
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Anmelden");
  await page.getByLabel(/Mandant/).fill(TENANT);
  await page.getByLabel(/E-Mail/).fill(EMAIL);
  await page.getByLabel(/Passwort/).fill(PASSWORD);
  await page.getByRole("button", { name: "Anmelden" }).click();
  await expect(page.getByRole("navigation", { name: "Hauptnavigation" })).toBeVisible();
}

/** createRecord fills a metadata-rendered form, addressing inputs by their
 * field id (stable) while asserting the visible label is German. */
async function createRecord(
  page: Page,
  resource: string,
  values: Record<string, string>,
) {
  await page.goto(`/o/${resource}/new`);
  for (const [field, value] of Object.entries(values)) {
    // Enums render as a <select> over their declared options since WP-1.11,
    // and fill() does not work on one.
    const control = page.locator(`#field-${field}`);
    if ((await control.evaluate((el) => el.tagName)) === "SELECT") {
      await control.selectOption(value);
    } else {
      await control.fill(value);
    }
  }
  const [response] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().endsWith(`/api/v1/${resource}`) && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Speichern" }).click(),
  ]);
  expect(response.status(), await response.text()).toBe(201);
  await page.waitForURL(new RegExp(`/o/${resource}/(?!new$)[^/]+$`));
  const id = page.url().split("/").pop()!;
  expect(id, "create did not navigate to the new record").not.toBe("new");
  return id;
}

test.describe.configure({ mode: "serial" });

test("the shell is localized: nav, object labels and the language switcher", async ({ page }) => {
  await signInGerman(page);

  const nav = page.getByRole("navigation", { name: "Hauptnavigation" });
  // Object names come from the server as machine names; the pack turns them
  // into words a German reader expects.
  await expect(nav.getByRole("link", { name: "Konto" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Kontakt" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Abmelden" })).toBeVisible();
  await scan(page, "shell (de)");

  // The switcher is a real control, not just a query parameter.
  const switcher = page.getByLabel("Sprache");
  await expect(switcher).toHaveValue("de");
  await switcher.selectOption("en");
  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible();
  // The nav's own accessible name is translated too, so it has to be found
  // again under its English one — which is the assertion, not an annoyance.
  await expect(
    page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Account" }),
  ).toBeVisible();

  // …and the choice survives a reload, with no query parameter in sight.
  await page.goto("/");
  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible();
  await page.getByLabel("Language").selectOption("de");
  await expect(page.getByRole("button", { name: "Abmelden" })).toBeVisible();
});

test("invoice lifecycle in German, including the PDF", async ({ page }) => {
  await signInGerman(page);
  const request = page.request;

  // --- metadata-rendered forms carry translated field labels ---
  await page.goto(`/o/account/new`);
  await expect(page.getByText("Kontonummer")).toBeVisible();
  await expect(page.getByText("Kontenart")).toBeVisible();
  await scan(page, "account form (de)");

  const arAccount = await createRecord(page, "account", {
    code: `11${RUN}`,
    name: "Accounts receivable",
    type: "asset",
  });
  const revenueAccount = await createRecord(page, "account", {
    code: `40${RUN}`,
    name: "Revenue",
    type: "income",
  });
  const taxAccount = await createRecord(page, "account", {
    code: `22${RUN}`,
    name: "Tax payable",
    type: "liability",
  });

  // A German-speaking customer: this is what the document's language comes from.
  const contact = await createRecord(page, "contact", {
    name: "Wile E. Kojote",
    email: `kojote-${RUN}@acme.test`,
    kind: "customer",
    locale: "de",
  });
  await scan(page, "contact detail (de)");

  const api = async (path: string, body: unknown) => {
    const response = await request.post(path, {
      data: body,
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
    expect(response.ok(), `${path} → ${response.status()} ${await response.text()}`).toBeTruthy();
    return response.json();
  };

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
        description_i18n: { de: "Raketenrollschuhe" },
        quantity: 2,
        unit_price_minor: 50000,
        revenue_account: revenueAccount,
        tax_jurisdiction: "DE",
        tax_category: "standard",
      },
    ],
  });

  // No locale was sent: the invoice took the customer's.
  expect(invoice.Locale, "invoice did not inherit the contact's language").toBe("de");

  // --- the invoice screen, in German ---
  await page.goto(`/invoices/${invoice.ID}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Rechnung");
  await expect(page.getByTestId("invoice-status")).toHaveText("draft");
  await expect(page.getByTestId("invoice-number")).toHaveText("—");
  await scan(page, "invoice draft (de)");

  await page.getByLabel(/Periode/).fill(period);
  const [postResponse] = await Promise.all([
    page.waitForResponse((r) => r.url().includes("/post") && r.request().method() === "POST"),
    page.getByRole("button", { name: "Im Hauptbuch buchen" }).click(),
  ]);
  expect(postResponse.status(), await postResponse.text()).toBe(200);

  await expect(page.getByTestId("invoice-status")).toHaveText("posted");
  await expect(page.getByTestId("invoice-number")).not.toHaveText("—");
  // 1000.00 net + 19% = 1190.00, rendered with the German decimal mark.
  await expect(page.getByTestId("invoice-total")).toContainText("1.190,00");
  await expect(page.getByRole("link", { name: "PDF herunterladen" })).toBeVisible();
  await scan(page, "invoice posted (de)");

  // --- and the document itself is German ---
  const pdf = await request.get(`/api/v1/invoices/${invoice.ID}/pdf`);
  expect(pdf.ok()).toBeTruthy();
  expect(pdf.headers()["content-type"]).toContain("application/pdf");
  const bytes = await pdf.body();
  expect(bytes.subarray(0, 4).toString()).toBe("%PDF");

  const text = bytes.toString("latin1"); // the page is WinAnsi-encoded
  for (const want of ["Rechnung", "Rechnungsdatum", "15.07.2026", "Raketenrollschuhe", "1.190,00"]) {
    expect(text, `PDF is missing ${want}`).toContain(want);
  }
  expect(text, "PDF still shows an English label").not.toContain("Issue date");
  // Währung: ä is a single 0xE4 byte in Windows-1252, not two UTF-8 bytes.
  expect(text).toContain("Währung");
});

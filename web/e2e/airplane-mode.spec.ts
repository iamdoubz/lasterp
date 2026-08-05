// SPDX-License-Identifier: AGPL-3.0-only
import { expect, test, type Page, type Request } from "@playwright/test";

// WP-2.7 / **Milestone M2**: docs/notes/M2-airplane-mode-script.md, executed.
//
// This is the milestone's acceptance criterion, not an illustration of it. Two
// things make it a test rather than a demo (WP-2.7-decisions.md §6):
//
//   - **The offline assertion is enforced.** Any request to /api/v1/ during the
//     offline phase fails the test. That is the one claim in the script a human
//     running the demo cannot check by looking, and it is the whole difference
//     between "the replica served this" and "the network happened to be fast".
//   - **Volume is part of the criterion.** One-of-each takes ninety seconds and
//     passing it would reproduce exactly the hollowness the premortem warned
//     about, so the offline day below is 20+ mutations interleaved with reads
//     and a reload.
//
// The replica's universe is Account and Contact — the two objects
// `crudObjects()` publishes to /meta/objects — so this is a chart-of-accounts
// and CRM day. That is the first two rows of docs/04's own offline-allowed
// list, not a compromise; see the script's §Not covered for invoices and GL
// posting, which are deliberately out of scope.

const TENANT = process.env.LASTERP_E2E_TENANT ?? "acme";
const EMAIL = process.env.LASTERP_E2E_EMAIL ?? "admin@example.com";
const PASSWORD = process.env.LASTERP_E2E_PASSWORD ?? "e2e-p4ssw0rd";

/** OFFLINE_DAY is how many contacts get created offline. The script calls for a
 * day's work rather than a gesture; this plus the edits and the account below
 * clears 20 mutations. */
const OFFLINE_DAY = 14;

const stamp = Date.now();
const tag = (n: number) => `Airplane ${stamp}-${n}`;

async function signIn(page: Page) {
  await page.goto("/");
  await page.getByLabel(/Tenant/).fill(TENANT);
  await page.getByLabel(/Email/).fill(EMAIL);
  await page.getByLabel(/Password/).fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

/** apiGuard fails the test when a **screen reads data from the API** while armed.
 *
 * It watches `request`, not `response`, so a call that is merely *attempted* and
 * fails because the browser is offline still trips it — the property is that the
 * data path does not point at the network, not that the network refused it.
 *
 * What it deliberately allows, because forbidding these would assert something
 * false rather than something strong:
 *
 *   - **the sync engine** (the /api/v1/sync routes) and **the drain** (writes to
 *     object routes) — the outbox is supposed to keep trying to send; that traffic
 *     failing offline and staying queued is the feature.
 *   - **the meta/objects and capabilities routes** - schema and session,
 *     fetched API-first with the replica's `_meta` as the fallback. One source
 *     and one cache of it, which WP-2.7-decisions.md §2 distinguishes from a
 *     second *data* path on purpose.
 *
 * What remains forbidden is exactly the regression worth catching: a **GET of an
 * object's rows**, which would mean a list or detail screen had been pointed
 * back at the server. */
function apiGuard(page: Page) {
  const seen: string[] = [];
  let armed = false;
  const dataRead = (method: string, path: string) =>
    method === "GET" && /^\/api\/v1\/(contact|account)(\/|$)/.test(path);
  const onRequest = (req: Request) => {
    const path = new URL(req.url()).pathname;
    if (armed && dataRead(req.method(), path)) {
      seen.push(`${req.method()} ${path}`);
    }
  };
  page.on("request", onRequest);
  return {
    arm: () => {
      armed = true;
    },
    disarm: () => {
      armed = false;
    },
    /** assertSilent is the enforced half of "no step reading the API". */
    assertSilent: (whileDoing: string) =>
      expect(seen, `${whileDoing}: the screens reached for the API while offline`).toEqual([]),
  };
}

/** SAVED matches the detail route a successful save lands on — and deliberately
 * excludes `/new`.
 *
 * `/o/contact/[^/]+$` matches `/o/contact/new` too, so an assertion built that
 * way passes *before the save has done anything* and a whole day of creates gets
 * verified by nothing. The negative lookahead is the difference between testing
 * the write and testing that a button exists. */
const SAVED = (resource: string) => new RegExp(`/o/${resource}/(?!new$)[^/]+$`);

async function createContact(page: Page, name: string) {
  await page.goto("/o/contact/new");
  await page.getByLabel(/Name/).first().fill(name);
  // `kind` is a required enum, and `conform` refuses an empty one at enqueue
  // time (INV-T5) rather than letting it reach the replica. Filling it is what
  // a user does; see WP-2.7-decisions.md §8 for the UX that refusal deserves.
  await page.getByLabel(/Kind/).first().selectOption("customer");
  await page.getByRole("button", { name: "Save" }).click();
  // Landing on the detail route is the optimistic apply: the row exists locally
  // and is addressable by an id the client minted (WP-2.3-decisions.md §2).
  await expect(page).toHaveURL(SAVED("contact"));
}

test.describe.configure({ mode: "serial" });

test("a day's work offline, converging on reconnect", async ({ page, context }) => {
  const guard = apiGuard(page);

  // --- Preconditions: the "issued laptop" moment -------------------------
  await signIn(page);
  await page.goto("/o/contact");
  await expect(page.getByRole("table")).toBeVisible();

  // The replica must actually hold something before the network goes away, or
  // every assertion below would pass just as well against an empty database —
  // the same trap the wipe tests had to be built around in WP-2.5.
  const seededOnline = await page.getByRole("row").count();
  expect(seededOnline, "fixture: the replica holds no rows, so going offline proves nothing")
    .toBeGreaterThan(1);

  // The app shell must be cached and the worker must be *controlling* this page
  // before the network goes away, or step 1's reload fails at the document
  // request. Waiting on `ready` rather than sleeping: registration happens on
  // load and activation is asynchronous, so a fixed delay would be a flake
  // waiting to happen on a slower machine.
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (navigator.serviceWorker.controller === null) {
      await new Promise((r) => navigator.serviceWorker.addEventListener("controllerchange", r, { once: true }));
    }
  });

  // --- The offline day ---------------------------------------------------
  await context.setOffline(true);
  guard.arm();

  // 1. A reload with no network. This is the step that separates a replica
  //    from an in-memory cache.
  await page.reload();
  await expect(page.getByRole("table")).toBeVisible();
  expect(await page.getByRole("row").count()).toBe(seededOnline);

  // 2–3. Read a row in full, offline.
  await page.getByRole("link", { name: "Open" }).first().click();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

  // 4. A day of creates.
  for (let i = 0; i < OFFLINE_DAY; i++) {
    await createContact(page, tag(i));
  }

  // 5. Edits to rows that came from the server.
  await page.goto("/o/contact");
  await page.getByRole("link", { name: "Open" }).first().click();
  await page.getByRole("link", { name: "Edit" }).click();
  await page.getByLabel(/Name/).first().fill(`Edited offline ${stamp}`);
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page).toHaveURL(SAVED("contact"));

  // 6. An Account, so the day spans both replicated objects.
  await page.goto("/o/account/new");
  await page.getByLabel(/Code/).first().fill(`9${String(stamp).slice(-5)}`);
  await page.getByLabel(/Name/).first().fill(`Airplane Account ${stamp}`);
  await page.getByLabel(/Account type/).first().selectOption("expense");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page).toHaveURL(SAVED("account"));

  // 7. Edit that account *before it has ever reached the server*. A design that
  //    minted a provisional id server-side would have nothing stable to target
  //    here; this is the step that proves it does not.
  const accountURL = page.url();
  await page.getByRole("link", { name: "Edit" }).click();
  await page.getByLabel(/Name/).first().fill(`Airplane Account ${stamp} (revised)`);
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page).toHaveURL(accountURL);

  // 8. Reload again. Everything above must survive, still unsent.
  await page.goto("/o/contact");
  await page.reload();
  await expect(page.getByTestId("pending-flag").first()).toBeVisible();
  const pendingBefore = await page.getByTestId("pending-flag").count();
  expect(pendingBefore, "the pending flag is not showing the offline work").toBeGreaterThan(0);

  // 9. Create then delete offline: the pair must resolve to nothing on the
  //    server rather than a create followed by a 404.
  await createContact(page, tag(900));
  await page.getByRole("button", { name: "Delete" }).click();
  await expect(page).toHaveURL(/\/o\/contact$/);

  // 10. A write only the server can refuse. The client does not re-validate
  //     business rules — the server is the referee (ADR-004) — so this is
  //     accepted locally and lands in the tray on reconnect.
  await page.goto("/o/contact/new");
  await page.getByLabel(/Name/).first().fill(`Airplane invalid ${stamp}`);
  await page.getByLabel(/Kind/).first().selectOption("customer");
  // A string the replica accepts and the server does not: the client never
  // re-validates business rules, so this is the only honest way to produce a
  // rejection (ADR-004 — the server is the referee).
  await page.getByLabel(/Email/).first().fill("not-an-address");
  await page.getByRole("button", { name: "Save" }).click();

  // The enforced claim: none of the above touched the API.
  guard.assertSilent("the offline day");
  guard.disarm();

  // --- Reconnect ---------------------------------------------------------
  await context.setOffline(false);
  await page.goto("/o/contact");

  // 13. The valid work lands. Poll rather than sleep: the drain is triggered by
  //     the reconnect and by the mount, and "eventually" is the honest contract.
  await expect(async () => {
    await page.reload();
    // The table first, then the flags. Asserting a count of zero on a page that
    // has not rendered its table yet passes for the wrong reason — which it did
    // on the first run of this spec, reporting a drained outbox over fourteen
    // rows that were still queued.
    await expect(page.getByRole("table")).toBeVisible();
    await expect(page.getByTestId("pending-flag")).toHaveCount(0);
  }).toPass({ timeout: 120_000 });

  // Every offline create is present, exactly once.
  for (let i = 0; i < OFFLINE_DAY; i++) {
    await expect(
      page.getByRole("cell", { name: tag(i), exact: true }),
      `${tag(i)} did not survive the reconnect exactly once`,
    ).toHaveCount(1);
  }
  // 9's create-then-delete resolved to nothing.
  await expect(page.getByRole("cell", { name: tag(900), exact: true })).toHaveCount(0);

  // 14. The refused command is in the tray, in the server's words.
  await page.goto("/sync");
  const tray = page.getByRole("table");
  await expect(tray).toBeVisible();
  await expect(tray.getByText(`Airplane invalid ${stamp}`)).toBeVisible();

  // 16. Nothing vanished: the tray holds exactly the commands the server
  //     refused, and the badge agrees.
  await expect(page.getByRole("row")).toHaveCount(2); // header + the one rejection
});

test("the offline guard fails when a screen reads the API", async ({ page, context }) => {
  // The mutation check for the check. `apiGuard` is the only thing standing
  // between this suite and a green run that proves nothing, so it gets its own
  // falsification: a deliberate API read while armed must be caught.
  await signIn(page);
  const guard = apiGuard(page);
  await context.setOffline(false);
  guard.arm();
  // A data read, which is the thing the guard exists to catch — not a schema
  // or sync call, which it allows by design.
  await page.evaluate(() => fetch("/api/v1/contact").catch(() => undefined));
  await expect(async () => {
    expect(() => guard.assertSilent("deliberate probe")).toThrow();
  }).toPass();
});

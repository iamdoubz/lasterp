// SPDX-License-Identifier: AGPL-3.0-only
import AxeBuilder from "@axe-core/playwright";
import { createHmac } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";

// WP-1.12 AC: enroll → confirm → sign in with a code → consume a recovery code
// → disable, in the Playwright suite. The HTTP-level half of the same criterion
// lives in internal/app/totp_integrity_test.go; this half proves a person can
// actually do it, which is the gap the WP exists to close — ValidateTOTP has
// shipped since WP-0.3 with no way for a user to turn it on.
//
// The test acts as the authenticator app: it takes the setup key the enrollment
// screen shows and computes RFC 6238 codes from it with node:crypto, the same
// way a phone would from the QR.

const TENANT = process.env.LASTERP_E2E_TENANT ?? "acme";
const EMAIL = process.env.LASTERP_E2E_EMAIL ?? "admin@example.com";
const PASSWORD = process.env.LASTERP_E2E_PASSWORD ?? "e2e-p4ssw0rd";

/** decodeBase32 turns the setup key back into the HMAC key. RFC 4648 alphabet,
 * unpadded — what identity.GenerateTOTPSecret emits. */
function decodeBase32(secret: string): Buffer {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const char of secret.toUpperCase().replace(/[^A-Z2-7]/g, "")) {
    bits += alphabet.indexOf(char).toString(2).padStart(5, "0");
  }
  const bytes: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) {
    bytes.push(parseInt(bits.slice(i, i + 8), 2));
  }
  return Buffer.from(bytes);
}

const STEP_MS = 30_000;

/** codeAt is RFC 6238 over HMAC-SHA1, 6 digits — the same parameters the
 * otpauth:// URI declares explicitly. */
function codeAt(secret: string, counter: number): string {
  const buf = Buffer.alloc(8);
  buf.writeBigUInt64BE(BigInt(counter));
  const mac = createHmac("sha1", decodeBase32(secret)).update(buf).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  const truncated = mac.readUInt32BE(offset) & 0x7fffffff;
  return String(truncated % 1_000_000).padStart(6, "0");
}

/** The last step this test spent. The server advances totp_last_counter on
 * every success — including the one that confirms enrollment — so a code is
 * good exactly once, and asking for another inside the same 30-second window
 * gets a replay rejection. That is the property being relied on, not worked
 * around. */
let burnedStep = -1;

/** freshCode waits for a window the server has not already seen, then returns
 * its code — which is what a person does: look at the app, and if it just used
 * that number, wait for the next one. */
async function freshCode(secret: string): Promise<string> {
  let step = Math.floor(Date.now() / STEP_MS);
  if (step <= burnedStep) {
    // Sleep to just past the next boundary rather than polling.
    const nextBoundary = (burnedStep + 1) * STEP_MS;
    await new Promise((resolve) => setTimeout(resolve, nextBoundary - Date.now() + 500));
    step = Math.floor(Date.now() / STEP_MS);
  }
  burnedStep = step;
  return codeAt(secret, step);
}

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

/** signIn fills the login form. secondFactor picks which field to use, which is
 * a deliberate choice in the UI rather than something the server sniffs out of
 * one field's shape (WP-1.12-decisions.md §5). */
async function signIn(
  page: Page,
  secondFactor?: { totp?: string; recoveryCode?: string },
) {
  await page.goto("/");
  await page.getByLabel(/Tenant/).fill(TENANT);
  await page.getByLabel(/Email/).fill(EMAIL);
  await page.getByLabel(/Password/).fill(PASSWORD);
  if (secondFactor?.recoveryCode) {
    await page.getByRole("button", { name: "Use a recovery code instead" }).click();
    await page.getByLabel(/Recovery code/).fill(secondFactor.recoveryCode);
  } else if (secondFactor?.totp) {
    await page.getByLabel(/Authentication code/).fill(secondFactor.totp);
  }
  await page.getByRole("button", { name: "Sign in" }).click();
}

async function signOut(page: Page) {
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
}

// Set once the account actually has a factor on. The suite shares one serial
// database and one admin account (playwright.config.ts: workers 1, no parallel),
// so a failure part-way through this test would otherwise leave MFA enabled and
// every later spec — and the CI retry, which starts with a password-only
// sign-in — would fail for a reason that has nothing to do with them.
let enrolledSecret: string | null = null;

test.afterAll(async ({ playwright }) => {
  if (!enrolledSecret) {
    return;
  }
  const api = await playwright.request.newContext({
    baseURL: process.env.LASTERP_E2E_PORT
      ? `http://localhost:${process.env.LASTERP_E2E_PORT}`
      : "http://localhost:8099",
  });
  try {
    const code = await freshCode(enrolledSecret);
    const login = await api.post("/api/v1/sessions", {
      data: { tenant: TENANT, email: EMAIL, password: PASSWORD, totp: code },
    });
    if (!login.ok()) {
      return;
    }
    await api.post("/api/v1/me/totp/disable", {
      headers: { "Idempotency-Key": crypto.randomUUID() },
      data: { password: PASSWORD, totp: await freshCode(enrolledSecret) },
    });
  } finally {
    await api.dispose();
    enrolledSecret = null;
  }
});

// The whole lifecycle is one test on purpose: each step's precondition is the
// previous step's effect, and splitting it would mean either re-driving the
// flow per test or leaving the shared tenant's admin account enrolled for
// whichever spec runs next.
test("second factor: enroll, confirm, sign in with a code, spend a recovery code, disable", async ({
  page,
}) => {
  // Three codes are spent, and each may have to wait out a 30-second window the
  // previous one already consumed.
  test.setTimeout(180_000);

  await signIn(page);
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();

  // --- enroll ---
  await page.getByRole("link", { name: "Account security" }).click();
  await expect(page.getByRole("heading", { name: "Account security" })).toBeVisible();
  await expect(page.getByText(/Two-factor authentication is off/)).toBeVisible();
  await scan(page, "account security, factor off");

  // Enrolling re-authenticates: a session alone must not be able to add a
  // factor its holder controls and the account owner does not.
  await page.getByLabel(/Password/).fill(PASSWORD);
  await page.getByRole("button", { name: "Turn on two-factor authentication" }).click();

  const setupKey = await page.getByLabel("Setup key").inputValue();
  enrolledSecret = setupKey;
  expect(setupKey).not.toEqual("");
  // The QR is decorative — the setup key beside it is its accessible
  // equivalent, and axe would flag a described bitmap as noise.
  await expect(page.locator('img[alt=""]')).toBeVisible();
  await scan(page, "enrollment, awaiting confirmation");

  // --- confirm ---
  await page.getByLabel("Authentication code").fill(await freshCode(setupKey));
  await page.getByRole("button", { name: "Confirm and turn on" }).click();

  const codeList = page.getByTestId("recovery-codes");
  await expect(codeList).toBeVisible();
  const recoveryCodes = await codeList.locator("li").allTextContents();
  expect(recoveryCodes).toHaveLength(10);
  await scan(page, "recovery codes");

  await page.getByRole("button", { name: "I have saved my codes" }).click();
  await expect(page.getByText(/Two-factor authentication is on/)).toBeVisible();
  await expect(page.getByText("10 recovery codes left")).toBeVisible();

  // --- sign in with a code ---
  await signOut(page);
  // A password alone is no longer enough.
  await signIn(page);
  await expect(page.getByText(/Sign-in failed/)).toBeVisible();

  // The next step, not the current one: confirming burned the code for the
  // window it was issued in, so it cannot double as a login code.
  await signIn(page, { totp: await freshCode(setupKey) });
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();

  // --- consume a recovery code ---
  await signOut(page);
  await signIn(page, { recoveryCode: recoveryCodes[0] });
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();

  await page.getByRole("link", { name: "Account security" }).click();
  await expect(page.getByText("9 recovery codes left")).toBeVisible();

  // AC: a recovery code cannot be reused.
  await signOut(page);
  await signIn(page, { recoveryCode: recoveryCodes[0] });
  await expect(page.getByText(/Sign-in failed/)).toBeVisible();

  await signIn(page, { recoveryCode: recoveryCodes[1] });
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();

  // --- disable ---
  await page.getByRole("link", { name: "Account security" }).click();
  await scan(page, "account security, factor on");

  // The session alone proves nothing: without a second factor this is refused.
  await page.getByLabel(/Password/).fill(PASSWORD);
  await page.getByRole("button", { name: "Turn off two-factor authentication" }).click();
  await expect(page.getByText(/were not accepted/)).toBeVisible();

  await page.getByLabel("Authentication code").fill(await freshCode(setupKey));
  await page.getByRole("button", { name: "Turn off two-factor authentication" }).click();
  await expect(page.getByText(/Two-factor authentication is off/)).toBeVisible();
  enrolledSecret = null; // the factor is off; afterAll has nothing to clean up

  // Back to a password-only account, which is where the test found it.
  await signOut(page);
  await signIn(page);
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
});

// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import App from "./App";
import { I18nProvider, resolveLocale, type LocaleId } from "./i18n";

// An unauthenticated App renders the login screen, which is what these tests
// drive. They carry the WP-0.7 i18n/RTL acceptance criteria forward onto the
// real client: pseudo-locale accents and direction still have to survive
// whatever the UI is made of.

beforeEach(() => {
  // Every call fails with a 401, so App settles on the login screen.
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve({
        ok: false,
        status: 401,
        statusText: "Unauthorized",
        json: () =>
          Promise.resolve({
            type: "about:blank",
            title: "authentication required",
            status: 401,
          }),
      }),
    ) as unknown as typeof fetch,
  );
});

afterEach(() => vi.unstubAllGlobals());

function renderAt(locale: LocaleId) {
  return render(
    <I18nProvider locale={resolveLocale(locale)}>
      <App />
    </I18nProvider>,
  );
}

test("an unauthenticated visitor gets the sign-in form", async () => {
  renderAt("en");
  await waitFor(() =>
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Sign in"),
  );
  expect(screen.getByLabelText(/Tenant/)).toBeInTheDocument();
  expect(screen.getByLabelText(/Email/)).toBeInTheDocument();
  expect(screen.getByLabelText(/Password/)).toBeInTheDocument();
});

test("english build renders translated strings and sets ltr", async () => {
  renderAt("en");
  await screen.findByRole("heading", { level: 1 });
  expect(document.documentElement.dir).toBe("ltr");
  expect(document.documentElement.lang).toBe("en");
});

test("pseudo-locale build renders accented text and is RTL (AC)", async () => {
  renderAt("pseudo");
  const heading = await screen.findByRole("heading", { level: 1 });
  // Accented pseudo-localization wraps every string in ⟦ … ⟧.
  expect(heading.textContent).toMatch(/^⟦.*⟧$/);
  // The AC pairs accents WITH RTL in one build.
  expect(document.documentElement.dir).toBe("rtl");
});

test("real RTL locale sets dir=rtl", async () => {
  renderAt("ar");
  await screen.findByRole("heading", { level: 1 });
  expect(document.documentElement.dir).toBe("rtl");
});

test("a failed sign-in shows one generic message, never which field was wrong", async () => {
  const user = userEvent.setup();
  renderAt("en");
  await screen.findByRole("heading", { level: 1 });

  await user.type(screen.getByLabelText(/Tenant/), "acme");
  await user.type(screen.getByLabelText(/Email/), "someone@example.com");
  await user.type(screen.getByLabelText(/Password/), "wrong");
  await user.click(screen.getByRole("button", { name: "Sign in" }));

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent("Sign-in failed. Check your details and try again.");
  // The server refuses to distinguish unknown-user from wrong-password; the UI
  // must not reintroduce the oracle by rendering the server's detail verbatim.
  expect(alert.textContent).not.toMatch(/user|email|account/i);
});

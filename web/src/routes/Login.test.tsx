// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { I18nProvider, resolveLocale } from "../i18n";
import { Login } from "./Login";

// The SSO button is offered on exactly one signal: the start route answering.
// There is no separate "is SSO configured?" flag to drift out of step with the
// server (WP-1.9-decisions.md §6).

function renderLogin() {
  return render(
    <I18nProvider locale={resolveLocale("en")}>
      <Login onSignedIn={() => {}} />
    </I18nProvider>,
  );
}

function mockFetch(response: Partial<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: false,
      status: 404,
      headers: new Headers({ "Content-Type": "application/problem+json" }),
      json: async () => ({ type: "about:blank", title: "not found", status: 404 }),
      text: async () => "",
      ...response,
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

test("the SSO button appears when a provider is configured", async () => {
  mockFetch({
    ok: true,
    status: 200,
    headers: new Headers({ "Content-Type": "application/json" }),
    json: async () => ({ authorization_url: "https://idp.example.com/authorize?state=abc" }),
  });

  renderLogin();

  const button = await screen.findByRole("button", { name: "Sign in with single sign-on" });
  expect(button).toBeTruthy();
  // The password form stays available: SSO is an additional way in, not a
  // replacement, and a deployment can run both.
  expect(screen.getByLabelText(/password/i)).toBeTruthy();
});

test("no SSO button when the deployment has no provider", async () => {
  mockFetch({ status: 404 });

  renderLogin();

  // The password form is what proves the screen finished rendering, so the
  // absence of the button below is a real absence and not a race.
  await waitFor(() => expect(screen.getByLabelText(/password/i)).toBeTruthy());
  expect(screen.queryByRole("button", { name: "Sign in with single sign-on" })).toBeNull();
});

// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import App from "./App";
import { I18nProvider, resolveLocale, type LocaleId } from "./i18n";

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve({
        json: () => Promise.resolve({ message: "hello from LastERP" }),
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

test("renders the hello message fetched from the kernel API", async () => {
  renderAt("en");
  await waitFor(() =>
    expect(screen.getByText("hello from LastERP")).toBeInTheDocument(),
  );
});

test("english build renders the translated tagline and sets ltr", async () => {
  renderAt("en");
  expect(
    screen.getByText("The last ERP anyone will need to build — or buy."),
  ).toBeInTheDocument();
  expect(document.documentElement.dir).toBe("ltr");
  await screen.findByText("hello from LastERP"); // flush fetch effect
});

test("pseudo-locale build renders accented text and is RTL (AC)", async () => {
  renderAt("pseudo");
  // Accented pseudo-localization wraps every string in ⟦ … ⟧.
  const heading = screen.getByRole("heading", { level: 1 });
  expect(heading.textContent).toMatch(/^⟦.*⟧$/);
  expect(heading.textContent).toContain("Ļàà"); // pseudo of "La…" in LastERP
  // The AC pairs accents WITH RTL in one build.
  expect(document.documentElement.dir).toBe("rtl");
  await screen.findByText("hello from LastERP"); // flush fetch effect
});

test("real RTL locale sets dir=rtl", async () => {
  renderAt("ar");
  expect(document.documentElement.dir).toBe("rtl");
  await screen.findByText("hello from LastERP"); // flush fetch effect
});

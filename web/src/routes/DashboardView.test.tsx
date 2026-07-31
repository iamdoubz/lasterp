// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";

import type { Card } from "../api";
import { I18nProvider, resolveLocale } from "../i18n";
import { KpiCard } from "./DashboardView";

function card(overrides: Partial<Card> = {}): Card {
  return {
    metric: "revenue",
    label: "Revenue",
    unit: "money_minor",
    grain: "flow",
    currency: "EUR",
    period: "2026-07",
    value: 1234500,
    good_direction: "up",
    ...overrides,
  };
}

function renderCard(c: Card, locale: "en" | "de" = "en") {
  return render(
    <I18nProvider locale={resolveLocale(locale)}>
      <KpiCard card={c} />
    </I18nProvider>,
  );
}

test("a money card renders its value and the period it is compared against", () => {
  renderCard(
    card({
      comparison: {
        basis: "prior_period",
        period: "2026-06",
        value: 1000000,
        delta_minor: 234500,
        delta_basis_points: 2345,
        improved: true,
      },
    }),
  );

  // Integer minor units are formatted at the very edge, never parsed into a
  // float for arithmetic.
  expect(screen.getByText(/12,345\.00/)).toBeInTheDocument();
  expect(screen.getByText(/\+23\.5%/)).toBeInTheDocument();
  expect(screen.getByText(/vs 2026-06/)).toBeInTheDocument();
});

// docs/21 §3: a lone number is impossible by default. When there genuinely is
// nothing to compare against, the card says so rather than implying a baseline.
test("a card with no comparison says so instead of showing a bare number", () => {
  renderCard(card());
  expect(screen.getByText(/No earlier period/i)).toBeInTheDocument();
});

// A percentage change from zero is not a number, so the card falls back to the
// absolute move rather than rendering an infinite or invented percentage.
test("a comparison against zero shows the absolute change", () => {
  renderCard(
    card({
      comparison: { basis: "prior_period", period: "2026-06", value: 0, delta_minor: 500000, improved: true },
    }),
  );
  expect(screen.getByText(/\+.*5,000\.00/)).toBeInTheDocument();
  expect(screen.queryByText(/%/)).not.toBeInTheDocument();
});

// Colour is never the only signal: the direction is also written as a sign.
test("a regression is signed, not merely coloured", () => {
  renderCard(
    card({
      metric: "ar_overdue",
      label: "Accounts receivable overdue",
      good_direction: "down",
      comparison: {
        basis: "prior_period",
        period: "2026-06",
        value: 100000,
        delta_minor: 50000,
        delta_basis_points: 5000,
        improved: false,
      },
    }),
  );
  const delta = screen.getByText(/\+50%/);
  expect(delta).toBeInTheDocument();
  expect(delta.className).toMatch(/rose/); // and it is coloured too
});

test("a count card is not formatted as money", () => {
  renderCard(card({ metric: "open_invoice_count", label: "Open invoices", unit: "count", value: 7, currency: undefined }));
  expect(screen.getByText("7")).toBeInTheDocument();
  expect(screen.queryByText(/€/)).not.toBeInTheDocument();
});

test("metric labels are translated, falling back to the server's label", () => {
  renderCard(card(), "de");
  expect(screen.getByText("Umsatz")).toBeInTheDocument();

  renderCard(card({ metric: "brand_new_metric", label: "Server label" }), "de");
  expect(screen.getByText("Server label")).toBeInTheDocument();
});

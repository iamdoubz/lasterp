// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@tanstack/react-query";

import { getDashboard, listDashboards, type Card, type Dashboard } from "../api";
import { useI18n } from "../i18n";
import { Alert, Busy } from "../ui";

/**
 * Role dashboards (docs/21 §3–4). The layout encodes the discipline rather than
 * leaving it to whoever assembles a view: the headline is dominant and
 * top-left, supporting tiles follow in the pack's declared order, and every card
 * carries its comparison — "a lone 4.2M is impossible by default".
 *
 * The grid, drag-drop and charts are WP-4.13. This renders what a pack declares.
 */

/** The currency dashboards are rendered in. One book, one reporting currency in
 * v1 — a currency picker belongs with the builder, and defaulting silently to
 * the wrong one would make every figure plausible and wrong. */
const CURRENCY = "EUR";

export function DashboardScreen({ name }: { name?: string }) {
  const { t } = useI18n();

  const catalog = useQuery({ queryKey: ["dashboards"], queryFn: listDashboards });

  // "Their role's dashboard is simply there": the pack matching one of the
  // viewer's roles wins, else the first they can actually render.
  const chosen =
    name ??
    catalog.data?.find((d) => d.suggested && d.available)?.name ??
    catalog.data?.find((d) => d.available)?.name;

  const dashboard = useQuery({
    queryKey: ["dashboard", chosen],
    queryFn: () => getDashboard(chosen!, CURRENCY),
    enabled: Boolean(chosen),
    retry: false,
  });

  if (catalog.isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (catalog.error) {
    return <Alert>{t("status.error")}</Alert>;
  }
  if (!chosen) {
    return <Alert tone="info">{t("dashboard.none")}</Alert>;
  }
  if (dashboard.isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (dashboard.error) {
    // The commonest cause by far is a book with no fiscal periods yet, which is
    // a setup step rather than a fault — say so instead of showing "something
    // went wrong" to someone who has simply not started.
    return <Alert tone="info">{t("dashboard.needsPeriods")}</Alert>;
  }

  return <DashboardBody dashboard={dashboard.data} />;
}

function DashboardBody({ dashboard }: { dashboard: Dashboard }) {
  const { t, label } = useI18n();
  const title = label(`dashboard.pack.${dashboard.name}`, dashboard.title);

  return (
    <section>
      <div className="mb-4">
        <h1 className="text-xl font-semibold text-slate-900 dark:text-slate-100">{title}</h1>
        <p className="text-sm text-slate-600 dark:text-slate-300">
          {t("dashboard.period", { period: dashboard.period })}
        </p>
      </div>

      {dashboard.headline && <KpiCard card={dashboard.headline} headline />}

      <div className="mt-4 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {dashboard.cards.map((card) => (
          <KpiCard key={card.metric} card={card} />
        ))}
      </div>

      {dashboard.omitted && dashboard.omitted.length > 0 && (
        <p className="mt-6 text-sm text-slate-600 dark:text-slate-300">
          {t("dashboard.omitted", { count: dashboard.omitted.length })}
        </p>
      )}
    </section>
  );
}

/**
 * KpiCard renders one measure.
 *
 * Colour never carries the meaning on its own: the delta is written with its
 * sign and an arrow, so the card reads identically in monochrome or to someone
 * who cannot distinguish the two greens (WCAG 1.4.1, which the axe gate does
 * not check for you).
 */
export function KpiCard({ card, headline = false }: { card: Card; headline?: boolean }) {
  const { t, label, formatMoney, formatNumber, formatPercent } = useI18n();
  const name = label(`metric.${card.metric}`, card.label);

  const value =
    card.unit === "money_minor" && card.currency
      ? formatMoney(card.value, card.currency)
      : formatNumber(card.value);

  const comparison = card.comparison;
  const tone = comparison?.improved === undefined
    ? "text-slate-600 dark:text-slate-300"
    : comparison.improved
      ? "text-emerald-700 dark:text-emerald-400"
      : "text-rose-700 dark:text-rose-400";

  return (
    <article
      className={`rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-700 dark:bg-slate-800 ${
        headline ? "sm:p-6" : ""
      }`}
    >
      <h2 className={`text-slate-600 dark:text-slate-300 ${headline ? "text-base" : "text-sm"}`}>
        {name}
      </h2>
      <p
        className={`mt-1 font-semibold text-slate-900 tabular-nums dark:text-slate-100 ${
          headline ? "text-4xl" : "text-2xl"
        }`}
      >
        {value}
      </p>

      {comparison ? (
        <p className={`mt-2 text-sm ${tone}`}>
          <span aria-hidden="true">{comparison.delta_minor >= 0 ? "▲ " : "▼ "}</span>
          {comparison.delta_basis_points === undefined
            ? deltaAmount(card, comparison.delta_minor, formatMoney, formatNumber)
            : formatPercent(comparison.delta_basis_points)}
          <span className="ms-2 text-slate-600 dark:text-slate-300">
            {t("dashboard.comparison", { period: comparison.period })}
          </span>
        </p>
      ) : (
        <p className="mt-2 text-sm text-slate-600 dark:text-slate-300">
          {t("dashboard.noComparison")}
        </p>
      )}
    </article>
  );
}

/** deltaAmount renders an absolute change, for the case where a percentage
 * would be a division by zero. */
function deltaAmount(
  card: Card,
  delta: number,
  formatMoney: (minor: number, currency: string) => string,
  formatNumber: (x: number) => string,
): string {
  const rendered =
    card.unit === "money_minor" && card.currency
      ? formatMoney(Math.abs(delta), card.currency)
      : formatNumber(Math.abs(delta));
  return (delta >= 0 ? "+" : "−") + rendered;
}

// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiError, getInvoice, idempotencyKey, invoicePdfUrl, postInvoice } from "../api";
import { useI18n } from "../i18n";
import { Alert, Busy, Button, buttonClass, Field, Input } from "../ui";

/**
 * The invoice screen is hand-built rather than metadata-rendered, and that is
 * the correct outcome, not a shortcut: Invoice is deliberately not a generic
 * CRUD object (WP-1.4b-decisions.md §2) because a generic PATCH would let a
 * client set status=posted with hand-picked totals, bypassing the posting
 * pipeline (INV-F5) and gapless numbering (INV-F6). Posting is a lifecycle verb
 * with its own route; the UI reflects that shape instead of hiding it.
 */
/** Placeholder for a value the server has not set yet. */
const EMPTY = "—";

function or(value: string | undefined): string {
  return value === undefined || value === "" ? EMPTY : value;
}

export function InvoiceDetail({ id }: { id: string }) {
  const { t, formatMoney } = useI18n();
  const queryClient = useQueryClient();
  const [period, setPeriod] = useState("");
  const [postKey] = useState(idempotencyKey);

  const { data, isPending, error } = useQuery({
    queryKey: ["invoice", id],
    queryFn: () => getInvoice(id),
  });

  const post = useMutation({
    mutationFn: () => postInvoice(id, period, postKey),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["invoice", id] });
    },
  });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("status.error")}</Alert>;
  }

  const posted = data.Status === "posted";
  const problem = post.error instanceof ApiError ? post.error.problem : undefined;
  const currency = String(data.Currency ?? "");

  return (
    <section>
      <h1 className="mb-4 text-xl font-semibold text-slate-900 dark:text-slate-100">
        {t("invoice.title")}
      </h1>

      {post.isError && <Alert>{problem?.detail || problem?.title || t("status.error")}</Alert>}
      {posted && data.GLEntryID && (
        <Alert tone="info">{t("invoice.posted", { entry: String(data.GLEntryID) })}</Alert>
      )}

      <dl className="mb-6 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="font-medium text-slate-700 dark:text-slate-200">{t("invoice.status")}</dt>
        <dd data-testid="invoice-status" className="text-slate-900 dark:text-slate-100">
          {data.Status}
        </dd>

        <dt className="font-medium text-slate-700 dark:text-slate-200">{t("invoice.number")}</dt>
        <dd data-testid="invoice-number" className="text-slate-900 dark:text-slate-100">
          {/* A draft has no number — the server returns "" for it, not null,
              so ?? would leave the cell blank instead of showing a placeholder.
              Numbers are allocated at acceptance, never at creation (INV-F6). */}
          {or(data.Number)}
        </dd>

        <dt className="font-medium text-slate-700 dark:text-slate-200">{t("invoice.total")}</dt>
        <dd data-testid="invoice-total" className="text-slate-900 dark:text-slate-100">
          {typeof data.GrossMinor === "number" && currency
            ? formatMoney(data.GrossMinor, currency)
            : EMPTY}
        </dd>

        {data.GLEntryID && (
          <>
            <dt className="font-medium text-slate-700 dark:text-slate-200">
              {t("invoice.journalEntry")}
            </dt>
            <dd data-testid="invoice-entry" className="text-slate-900 dark:text-slate-100">
              {String(data.GLEntryID)}
            </dd>
          </>
        )}
      </dl>

      {!posted && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            post.mutate();
          }}
        >
          <Field id="period" label={t("invoice.period")} required>
            <Input
              id="period"
              required
              value={period}
              onChange={(e) => setPeriod(e.target.value)}
            />
          </Field>
          <Button type="submit" variant="primary" disabled={post.isPending} data-testid="post-invoice">
            {post.isPending ? t("invoice.posting") : t("invoice.post")}
          </Button>
        </form>
      )}

      {posted && (
        <a href={invoicePdfUrl(id)} data-testid="invoice-pdf" className={buttonClass()}>
          {t("invoice.pdf")}
        </a>
      )}
    </section>
  );
}

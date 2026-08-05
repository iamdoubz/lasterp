// SPDX-License-Identifier: AGPL-3.0-only

import { Link } from "@tanstack/react-router";

import { type MetaObject } from "../api";
import { useI18n } from "../i18n";
import { formatValue, labelFor, listFields, objectLabel } from "../meta/render";
import { useRecords } from "../sync/ReplicaContext";
import { Alert, Busy, buttonClass, Table } from "../ui";

/** The list view for any object, rendered entirely from its schema. */
export function ObjectList({ object }: { object: MetaObject }) {
  const { t, label, locale, formatMoney, formatNumber } = useI18n();
  const fields = listFields(object.fields);
  const name = objectLabel(object.name, label);

  // The replica, not the API — WP-2.7-decisions.md §2. Same call online and
  // off; the only difference is how recently `sync()` last caught up.
  const { data, isPending, error } = useRecords(object.name);

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    // A replica that has never reached the server has no schema and no rows,
    // and there is no API to fall back to by design. Say what would fix it.
    return <Alert>{t("object.offline.unavailable")}</Alert>;
  }

  const { rows, pending } = data;

  const columns = [
    ...fields.map((f) => labelFor(f, object.name, label)),
    t("object.column.actions"),
  ];

  return (
    <section>
      <div className="mb-4 flex items-center justify-between gap-4">
        <h1 className="text-xl font-semibold text-slate-900 dark:text-slate-100">
          {t("object.list.title", { object: name })}
        </h1>
        <Link
          to="/o/$resource/new"
          params={{ resource: object.resource }}
          className={buttonClass("primary")}
        >
          {t("object.list.new", { object: name })}
        </Link>
      </div>

      {rows.length === 0 ? (
        <p className="text-sm text-slate-500 dark:text-slate-400">{t("object.list.empty")}</p>
      ) : (
        <Table caption={t("object.list.title", { object: name })} columns={columns}>
          {rows.map((record) => {
            const id = String(record.id ?? "");
            return (
              <tr key={id} className="border-b border-slate-200 dark:border-slate-700">
                {fields.map((f, i) => (
                  <td key={f.name} className="px-3 py-2 text-slate-900 dark:text-slate-100">
                    {formatValue(f, record[f.name], record, {
                      money: formatMoney,
                      number: formatNumber,
                      locale: locale.tag,
                    })}
                    {/* The pending flag rides the first column so it reads as
                        part of the row rather than as a column of its own that
                        is empty for almost every row. */}
                    {i === 0 && pending.has(id) && (
                      <span
                        data-testid="pending-flag"
                        className="ml-2 rounded bg-amber-100 px-1.5 py-0.5 text-xs font-medium text-amber-900 dark:bg-amber-900 dark:text-amber-100"
                      >
                        {t("object.row.pending")}
                      </span>
                    )}
                  </td>
                ))}
                <td className="px-3 py-2">
                  <Link
                    to="/o/$resource/$id"
                    params={{ resource: object.resource, id }}
                    // min-h/inline-flex keeps the tap target at 24×24 even
                    // though the text is short (WCAG 2.2 target size).
                    className="inline-flex min-h-11 items-center text-sky-700 underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-600 dark:text-sky-400"
                  >
                    {t("object.list.open")}
                  </Link>
                </td>
              </tr>
            );
          })}
        </Table>
      )}
    </section>
  );
}

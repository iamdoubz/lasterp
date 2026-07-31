// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { listRecords, type MetaObject } from "../api";
import { useI18n } from "../i18n";
import { formatValue, labelFor, listFields } from "../meta/render";
import { Alert, Busy, buttonClass, Table } from "../ui";

/** The list view for any object, rendered entirely from its schema. */
export function ObjectList({ object }: { object: MetaObject }) {
  const { t, formatMoney, formatNumber } = useI18n();
  const fields = listFields(object.fields);

  const { data, isPending, error } = useQuery({
    queryKey: ["records", object.resource],
    queryFn: () => listRecords(object.resource),
  });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("status.error")}</Alert>;
  }

  const columns = [...fields.map(labelFor), t("object.column.actions")];

  return (
    <section>
      <div className="mb-4 flex items-center justify-between gap-4">
        <h1 className="text-xl font-semibold text-slate-900 dark:text-slate-100">
          {t("object.list.title", { object: object.name })}
        </h1>
        <Link
          to="/o/$resource/new"
          params={{ resource: object.resource }}
          className={buttonClass("primary")}
        >
          {t("object.list.new", { object: object.name })}
        </Link>
      </div>

      {data.length === 0 ? (
        <p className="text-sm text-slate-500 dark:text-slate-400">{t("object.list.empty")}</p>
      ) : (
        <Table caption={t("object.list.title", { object: object.name })} columns={columns}>
          {data.map((record) => {
            const id = String(record.id ?? "");
            return (
              <tr key={id} className="border-b border-slate-200 dark:border-slate-700">
                {fields.map((f) => (
                  <td key={f.name} className="px-3 py-2 text-slate-900 dark:text-slate-100">
                    {formatValue(f, record[f.name], record, {
                      money: formatMoney,
                      number: formatNumber,
                    })}
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

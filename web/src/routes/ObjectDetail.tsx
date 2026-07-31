// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { getRecord, type MetaObject } from "../api";
import { useI18n } from "../i18n";
import { editableFields, formatValue, labelFor, objectLabel } from "../meta/render";
import { Alert, Busy, buttonClass } from "../ui";

/** The detail view for any object: a description list rendered from schema. */
export function ObjectDetail({ object, id }: { object: MetaObject; id: string }) {
  const { t, label, locale, formatMoney, formatNumber } = useI18n();
  const name = objectLabel(object.name, label);

  const { data, isPending, error } = useQuery({
    queryKey: ["record", object.resource, id],
    queryFn: () => getRecord(object.resource, id),
  });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("status.error")}</Alert>;
  }

  return (
    <section>
      <div className="mb-4 flex items-center justify-between gap-4">
        <h1 className="text-xl font-semibold text-slate-900 dark:text-slate-100">
          {t("object.detail.title", { object: name })}
        </h1>
        <div className="flex gap-2">
          <Link to="/o/$resource" params={{ resource: object.resource }} className={buttonClass()}>
            {t("object.detail.back")}
          </Link>
          <Link
            to="/o/$resource/$id/edit"
            params={{ resource: object.resource, id }}
            className={buttonClass("primary")}
          >
            {t("object.detail.edit")}
          </Link>
        </div>
      </div>

      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        {editableFields(object.fields).map((f) => (
          <div key={f.name} className="contents">
            <dt className="font-medium text-slate-700 dark:text-slate-200">{labelFor(f, object.name, label)}</dt>
            <dd className="text-slate-900 dark:text-slate-100">
              {formatValue(f, data[f.name], data, {
                money: formatMoney,
                number: formatNumber,
                locale: locale.tag,
              })}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

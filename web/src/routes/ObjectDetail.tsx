// SPDX-License-Identifier: AGPL-3.0-only

import { Link, useNavigate } from "@tanstack/react-router";

import { type MetaObject } from "../api";
import { useI18n } from "../i18n";
import { editableFields, formatValue, labelFor, objectLabel } from "../meta/render";
import { newId } from "../sync/ids";
import { useRecords, useWriteRecord } from "../sync/ReplicaContext";
import { Alert, Busy, Button, buttonClass } from "../ui";

/** The detail view for any object: a description list rendered from schema. */
export function ObjectDetail({ object, id }: { object: MetaObject; id: string }) {
  const { t, label, locale, formatMoney, formatNumber } = useI18n();
  const name = objectLabel(object.name, label);
  const navigate = useNavigate();
  const remove = useWriteRecord(object.name);

  // Deliberately the same query as the list, not a per-row read: it shares the
  // list's cache, so opening a row after a list costs nothing and the two can
  // never show different values for the same field.
  const { data, isPending, error } = useRecords(object.name);

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("object.offline.unavailable")}</Alert>;
  }

  const record = data.rows.find((r) => String(r.id ?? "") === id);
  if (record === undefined) {
    return <Alert>{t("status.error")}</Alert>;
  }

  function destroy() {
    remove.mutate(
      {
        commandId: newId(),
        method: "DELETE",
        object: object.name,
        rowId: id,
        body: null,
      },
      { onSuccess: () => navigate({ to: "/o/$resource", params: { resource: object.resource } }) },
    );
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
          <Button variant="danger" onClick={destroy} disabled={remove.isPending}>
            {remove.isPending ? t("object.detail.deleting") : t("object.detail.delete")}
          </Button>
        </div>
      </div>

      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        {editableFields(object.fields).map((f) => (
          <div key={f.name} className="contents">
            <dt className="font-medium text-slate-700 dark:text-slate-200">{labelFor(f, object.name, label)}</dt>
            <dd className="text-slate-900 dark:text-slate-100">
              {formatValue(f, record[f.name], record, {
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

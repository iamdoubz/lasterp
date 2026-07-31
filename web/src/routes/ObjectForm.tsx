// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";

import {
  ApiError,
  createRecord,
  idempotencyKey,
  updateRecord,
  type MetaObject,
  type Record_,
} from "../api";
import { useT } from "../i18n";
import { FieldControl, editableFields, emptyRecord, labelFor, submittable } from "../meta/render";
import { Alert, Button, Field } from "../ui";

interface Props {
  object: MetaObject;
  /** Present when editing; absent for a new record. */
  id?: string;
  initial?: Record_;
}

/** The create/edit form for any object, rendered from its schema. */
export function ObjectForm({ object, id, initial }: Props) {
  const t = useT();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const fields = editableFields(object.fields);

  const [values, setValues] = useState<Record_>(() => ({
    ...emptyRecord(object.fields),
    ...(initial ?? {}),
  }));

  // One key per mounted form, not per submit: a retry after a network blip is
  // the same logical write and must not create a second record (ADR-009).
  const [writeKey] = useState(idempotencyKey);

  const save = useMutation({
    mutationFn: (body: Record_) =>
      id
        ? updateRecord(object.resource, id, body, writeKey)
        : createRecord(object.resource, body, writeKey),
    onSuccess: async (saved) => {
      await queryClient.invalidateQueries({ queryKey: ["records", object.resource] });
      navigate({
        to: "/o/$resource/$id",
        params: { resource: object.resource, id: String(saved.id ?? id ?? "") },
      });
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate(submittable(object.fields, values));
  }

  const problem = save.error instanceof ApiError ? save.error.problem : undefined;

  return (
    <section>
      <h1 className="mb-4 text-xl font-semibold text-slate-900 dark:text-slate-100">
        {id
          ? t("object.form.editTitle", { object: object.name })
          : t("object.form.newTitle", { object: object.name })}
      </h1>

      {save.isError && (
        <Alert>{problem?.detail || problem?.title || t("status.error")}</Alert>
      )}

      <form onSubmit={submit} noValidate>
        {fields.map((f) => (
          <Field key={f.name} id={`field-${f.name}`} label={labelFor(f)} required={f.required}>
            <FieldControl
              field={f}
              id={`field-${f.name}`}
              value={values[f.name]}
              onChange={(v) => setValues((prev) => ({ ...prev, [f.name]: v }))}
            />
          </Field>
        ))}

        <div className="flex gap-2">
          <Button type="submit" variant="primary" disabled={save.isPending}>
            {save.isPending ? t("object.form.saving") : t("object.form.save")}
          </Button>
          <Button onClick={() => navigate({ to: "/o/$resource", params: { resource: object.resource } })}>
            {t("object.form.cancel")}
          </Button>
        </div>
      </form>
    </section>
  );
}

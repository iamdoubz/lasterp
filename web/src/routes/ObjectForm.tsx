// SPDX-License-Identifier: AGPL-3.0-only

import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";

import { type MetaObject, type Record_ } from "../api";
import { useI18n } from "../i18n";
import {
  FieldControl,
  editableFields,
  emptyRecord,
  groupFields,
  labelFor,
  objectLabel,
  submittable,
} from "../meta/render";
import { newId } from "../sync/ids";
import { useWriteRecord } from "../sync/ReplicaContext";
import { Alert, Button, Field } from "../ui";

interface Props {
  object: MetaObject;
  /** Present when editing; absent for a new record. */
  id?: string;
  initial?: Record_;
}

/** The create/edit form for any object, rendered from its schema. */
export function ObjectForm({ object, id, initial }: Props) {
  const { t, label } = useI18n();
  const name = objectLabel(object.name, label);
  const navigate = useNavigate();
  // editableFields already applies the schema's field order; groupFields then
  // splits it into sections without disturbing that order within each.
  const groups = groupFields(editableFields(object.fields));

  const [values, setValues] = useState<Record_>(() => ({
    ...emptyRecord(object.fields),
    ...(initial ?? {}),
  }));

  // One id per mounted form, not per submit: a retry after a network blip is
  // the same logical write and must not create a second record. That reasoning
  // is WP-1.5's and survives verbatim; only the identifier changed. The
  // command_id **is** the Idempotency-Key when the drain replays this
  // (WP-2.3-decisions.md §1), so the property is now enforced by the outbox
  // rather than by this component remembering to hold a key.
  const [commandId] = useState(newId);
  // For a create the client owns the row id (WP-2.3-decisions.md §2), so the
  // row the user sees offline is the row the server ends up with — and an edit
  // made before it has ever been sent has something stable to target.
  const [newRowId] = useState(newId);
  const rowId = id ?? newRowId;

  // The outbox, not the API — there is no online variant. A command is a stored
  // HTTP request replayed through the ordinary route, so saving online and
  // saving offline are the same code reaching the same endpoint; only the delay
  // before the drain differs (WP-2.7-decisions.md §3).
  const save = useWriteRecord(object.name);

  function submit(e: FormEvent) {
    e.preventDefault();
    const body = submittable(object.fields, values);
    save.mutate(
      {
        commandId,
        method: id ? "PATCH" : "POST",
        object: object.name,
        rowId,
        // The id travels in the body on a create: it is the server accepting a
        // client-supplied row id, not a path parameter.
        body: id ? body : { ...body, id: rowId },
      },
      {
        onSuccess: () =>
          navigate({ to: "/o/$resource/$id", params: { resource: object.resource, id: rowId } }),
      },
    );
  }

  return (
    <section>
      <h1 className="mb-4 text-xl font-semibold text-slate-900 dark:text-slate-100">
        {id
          ? t("object.form.editTitle", { object: name })
          : t("object.form.newTitle", { object: name })}
      </h1>

      {/* A queued write fails only for local reasons — a full outbox, a locked
          replica, a value the schema refuses. Server rejections are not errors
          here: the command is durable, and the server's answer arrives in the
          conflict tray (INV-S4) rather than under this form. */}
      {save.isError && (
        <Alert>{save.error instanceof Error ? save.error.message : t("status.error")}</Alert>
      )}

      <form onSubmit={submit} noValidate>
        {groups.map((group) => {
          const controls = group.fields.map((f) => (
            <Field key={f.name} id={`field-${f.name}`} label={labelFor(f, object.name, label)} required={f.required}>
              <FieldControl
                field={f}
                id={`field-${f.name}`}
                object={object.name}
                label={label}
                value={values[f.name]}
                onChange={(v) => setValues((prev) => ({ ...prev, [f.name]: v }))}
              />
            </Field>
          ));
          // An ungrouped schema renders exactly as it did before descriptors
          // existed — no fieldset, no legend, no extra landmark for a screen
          // reader to announce.
          if (group.name === "") {
            return <div key="__ungrouped">{controls}</div>;
          }
          return (
            <fieldset key={group.name} className="mb-4 border-t border-slate-200 pt-4 dark:border-slate-700">
              <legend className="pe-2 text-sm font-semibold text-slate-900 dark:text-slate-100">
                {label(`schema.group.${object.name}.${group.name}`, group.name)}
              </legend>
              {controls}
            </fieldset>
          );
        })}

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

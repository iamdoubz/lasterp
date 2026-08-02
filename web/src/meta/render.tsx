// SPDX-License-Identifier: AGPL-3.0-only

// The metadata renderer: "metadata-rendered list/form/detail" is the total
// functions below over the kernel's closed FieldType set (WP-1.5-decisions.md
// §2). WP-1.11 gave the schema the UI descriptors that WP deferred — field
// order, grouping and widget overrides — plus enum option sets, so an enum is
// now a select over the same closed list the server validates against instead
// of a free-text box and a 422.

import type { ChangeEvent } from "react";

import { SYSTEM_FIELDS, type FieldType, type MetaField, type Record_ } from "../api";
import { Checkbox, Input, Radio, Select, Textarea } from "../ui";

/** A Labeler resolves a catalog key that may not exist, falling back to a
 * humanized default — `useI18n().label`. */
export type Labeler = (key: string, fallback: string) => string;

/** humanize turns a field name into a readable default ("issue_date" → "Issue
 * date"). It is the fallback, not the answer: a schema key wins when the pack
 * has one. */
export function humanize(name: string): string {
  const words = name.replace(/_/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/**
 * labelFor is a field's label in the reader's language (docs/17: "metadata
 * labels flow through the translation layer").
 *
 * The key is derived from the schema — `schema.field.<Object>.<field>` — so a
 * pack can name every shipped field, while a field with no key (an overlay's
 * custom field, a module newer than this bundle) still renders its humanized
 * name rather than a raw key. Per-tenant relabelling is overlay work and needs
 * labels in the object schema first (WP-1.7-decisions.md §11).
 */
export function labelFor(field: MetaField, object: string, label: Labeler): string {
  return label(`schema.field.${object}.${field.name}`, humanize(field.name));
}

/** objectLabel is an object's name in the reader's language. */
export function objectLabel(object: string, label: Labeler): string {
  return label(`schema.object.${object}`, object);
}

/**
 * optionLabel is one enum option in the reader's language.
 *
 * Option *values* are data — the server stores and validates them — but what a
 * person reads is a UI string, so they resolve the same way field labels do:
 * a schema key when the pack has one, a humanized fallback when it does not.
 * A pack that has not caught up with a new option shows "Both", not a raw key.
 */
export function optionLabel(
  field: MetaField,
  object: string,
  value: string,
  label: Labeler,
): string {
  return label(`schema.option.${object}.${field.name}.${value}`, humanize(value));
}

/**
 * orderedFields sorts by the schema's `order`, then declaration index.
 *
 * Unset order is 0, so a schema that declares none renders exactly as it did
 * before descriptors existed, and an overlay can slot a new field between two
 * core ones by numbering it. Array.prototype.sort is stable in every engine
 * this targets, which is what makes the declaration-index tiebreak implicit.
 */
export function orderedFields(fields: MetaField[]): MetaField[] {
  return [...fields].sort((a, b) => (a.order ?? 0) - (b.order ?? 0));
}

/** editableFields drops the columns the server owns. A form that posted
 * created_at or tenant_id would be rejected (DisallowUnknownFields) at best and
 * a tenancy bug at worst. Ordering happens here so every consumer inherits it
 * without having to remember to sort. */
export function editableFields(fields: MetaField[]): MetaField[] {
  return orderedFields(fields).filter(
    (f) => !SYSTEM_FIELDS.includes(f.name) && f.type !== "computed",
  );
}

/** listFields is what a list view shows: the first few editable fields, so a
 * wide object doesn't produce a table nobody can read. Which few is now the
 * schema's call rather than declaration order's. */
export function listFields(fields: MetaField[], limit = 5): MetaField[] {
  return editableFields(fields).slice(0, limit);
}

/** A group of fields rendered together under one legend. Fields with no group
 * come first, in one unnamed group, so an ungrouped schema renders as a plain
 * list of controls exactly as before. */
export interface FieldGroup {
  /** "" for the ungrouped fields. */
  name: string;
  fields: MetaField[];
}

/** groupFields splits fields into sections, preserving first-appearance order
 * of the groups themselves. */
export function groupFields(fields: MetaField[]): FieldGroup[] {
  const groups: FieldGroup[] = [];
  const byName = new Map<string, FieldGroup>();
  for (const f of fields) {
    const name = f.group ?? "";
    let group = byName.get(name);
    if (!group) {
      group = { name, fields: [] };
      byName.set(name, group);
      groups.push(group);
    }
    group.fields.push(f);
  }
  // The ungrouped section, if any, leads: it holds the object's identifying
  // fields in every schema that groups at all.
  return groups.sort((a, b) => (a.name === "" ? -1 : b.name === "" ? 1 : 0));
}

/**
 * formatValue renders a stored value for display.
 *
 * Money is the case that matters: values arrive as exact strings of integer
 * minor units and are formatted through Intl at the very edge. They are never
 * parsed into a JS number for arithmetic — `0.1 + 0.2` is exactly the class of
 * bug the integer-minor-unit rule exists to prevent (CLAUDE.md).
 */
export function formatValue(
  field: MetaField,
  value: unknown,
  record: Record_,
  format: {
    money: (minor: number, currency: string) => string;
    number: (x: number) => string;
    /** The reader's locale, for fields whose value exists in several. */
    locale?: string;
  },
): string {
  if (field.localized && format.locale) {
    const translated = translationFor(record, field.name, format.locale);
    if (translated) {
      return translated;
    }
  }
  if (value === null || value === undefined || value === "") {
    return "";
  }
  switch (field.type) {
    case "money": {
      const minor = Number(value);
      const currency = String(record.currency ?? record.Currency ?? "");
      // Without a currency there is no correct way to place the decimal point;
      // show the raw minor units rather than guess and be wrong.
      if (!currency || !Number.isFinite(minor)) {
        return String(value);
      }
      return format.money(minor, currency);
    }
    case "int":
      return Number.isFinite(Number(value)) ? format.number(Number(value)) : String(value);
    case "bool":
      return value ? "✓" : "—";
    case "json":
    case "address":
    case "table":
      return JSON.stringify(value);
    default:
      // decimal/percent stay exact strings; date/datetime are already
      // ISO-formatted UTC from the server.
      return String(value);
  }
}

/**
 * translationFor reads a record's per-locale value for a field: exact tag
 * first, then the base language, so a "de" translation serves a "de-AT" reader.
 * Empty when the record has none — the caller shows the canonical value.
 *
 * Forms deliberately do *not* use this: they edit the canonical field, so
 * saving a form in German cannot overwrite the English value with a German one.
 * Editing translations needs a control per locale, which is UI-descriptor work
 * (WP-1.7-decisions.md §11).
 */
export function translationFor(record: Record_, field: string, locale: string): string {
  const translations = record.translations as
    | Record<string, Record<string, string> | undefined>
    | undefined;
  const byLocale = translations?.[field];
  if (!byLocale) {
    return "";
  }
  return byLocale[locale] || byLocale[locale.split("-")[0]] || "";
}

interface ControlProps {
  field: MetaField;
  id: string;
  value: unknown;
  onChange: (value: unknown) => void;
  /** The owning object, for resolving enum option labels. Optional so the
   * control stays renderable without an i18n context. */
  object?: string;
  label?: Labeler;
}

/**
 * FieldControl is the FieldType → control map. It is exhaustive over FieldType
 * by construction: the `never` check at the bottom makes adding a kernel field
 * type a compile error here rather than a silently unrendered form.
 *
 * A field's `widget` overrides that map where the schema asks for it. The set
 * is closed and the server validates applicability, so an unknown widget never
 * reaches here — but the fallback is still the type's own control rather than
 * nothing, because a schema from a newer server than this bundle is a
 * deployment reality.
 */
export function FieldControl({ field, id, value, onChange, object, label }: ControlProps) {
  const str = value === null || value === undefined ? "" : String(value);
  const text = (e: ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) =>
    onChange(e.target.value);
  const optionText = (v: string) =>
    object && label ? optionLabel(field, object, v, label) : humanize(v);

  if (field.widget === "textarea") {
    return <Textarea id={id} value={str} required={field.required} onChange={text} />;
  }
  if (field.widget === "radio" && field.options) {
    return (
      <div role="radiogroup" aria-labelledby={`${id}-label`} className="flex flex-col gap-1">
        {field.options.map((option) => (
          <Radio
            key={option}
            id={`${id}-${option}`}
            name={id}
            value={option}
            checked={str === option}
            label={optionText(option)}
            onChange={text}
          />
        ))}
      </div>
    );
  }

  switch (field.type) {
    case "long_text":
    case "rich_text":
      // rich_text renders as plain text in v1: a rich editor is a dependency
      // and a sanitization surface, neither of which any Phase-1 screen needs.
      return <Textarea id={id} value={str} required={field.required} onChange={text} />;

    case "bool":
      return (
        <Checkbox
          id={id}
          checked={Boolean(value)}
          onChange={(e) => onChange(e.target.checked)}
        />
      );

    case "int":
      return (
        <Input
          id={id}
          type="number"
          step={1}
          value={str}
          required={field.required}
          onChange={(e) => onChange(e.target.value === "" ? "" : Number(e.target.value))}
        />
      );

    case "money":
    case "decimal":
    case "percent":
      // Exact decimal strings, never a number input: type="number" hands back a
      // JS float and would round-trip 0.07 into 0.07000000000000001.
      return (
        <Input
          id={id}
          type="text"
          inputMode="decimal"
          value={str}
          required={field.required}
          onChange={text}
        />
      );

    case "date":
      return <Input id={id} type="date" value={str.slice(0, 10)} required={field.required} onChange={text} />;

    case "datetime":
      return <Input id={id} type="datetime-local" value={str.slice(0, 16)} required={field.required} onChange={text} />;

    case "email":
      return <Input id={id} type="email" value={str} required={field.required} onChange={text} />;

    case "phone":
      return <Input id={id} type="tel" value={str} required={field.required} onChange={text} />;

    case "duration":
      return <Input id={id} type="text" value={str} required={field.required} onChange={text} />;

    case "currency":
      // ISO-4217 alpha-3. A full currency picker needs the registry endpoint
      // (kernel/money) that no route exposes yet.
      return (
        <Input
          id={id}
          type="text"
          maxLength={3}
          value={str}
          required={field.required}
          onChange={(e) => onChange(e.target.value.toUpperCase())}
        />
      );

    case "enum":
      // The schema carries the closed set (WP-1.11), so offer exactly it. An
      // enum with no options cannot reach here — the server refuses to parse
      // such a schema — but a bundle older than its server might see one, and
      // a free-text box is a better failure than an empty select.
      if (!field.options?.length) {
        return <Input id={id} type="text" value={str} required={field.required} onChange={text} />;
      }
      return (
        <Select id={id} value={str} required={field.required} onChange={text}>
          {/* An optional enum needs a way back to "unset"; a required one is
              placeholder-only until the user picks, so the browser's own
              required-field validation has something to catch. */}
          <option value="">{field.required ? "—" : ""}</option>
          {field.options.map((option) => (
            <option key={option} value={option}>
              {optionText(option)}
            </option>
          ))}
        </Select>
      );

    case "link":
      // A reference picker needs to list the target object; that is a second
      // query per field and belongs with the UI descriptor work. The id is
      // accepted directly, which is what the API takes anyway.
      return (
        <Input
          id={id}
          type="text"
          value={str}
          required={field.required}
          placeholder={field.target}
          onChange={text}
        />
      );

    case "json":
    case "address":
    case "table":
      return (
        <Textarea
          id={id}
          value={typeof value === "string" ? value : JSON.stringify(value ?? "", null, 2)}
          required={field.required}
          onChange={text}
        />
      );

    case "file":
      // Uploads need a blob endpoint; none exists in Phase 1.
      return <Input id={id} type="text" value={str} required={field.required} onChange={text} />;

    case "text":
      return <Input id={id} type="text" value={str} required={field.required} onChange={text} />;

    case "computed":
      // Server-derived: shown, never edited.
      return <Input id={id} type="text" value={str} readOnly disabled />;

    default:
      return assertNever(field.type, id, str);
  }
}

/** assertNever fails the build when a new FieldType lands in the kernel without
 * a control here. It still renders something at runtime, because a schema from
 * a newer server than this bundle is a deployment reality, not a bug. */
function assertNever(type: never, id: string, value: string) {
  console.warn(`no control for field type ${String(type)}; falling back to text`);
  return <Input id={id} type="text" defaultValue={value} />;
}

/** A Select is exported for screens with a closed option set of their own
 * (invoice period, capability toggles) that does not come from a schema. */
export { Select };

/** emptyRecord builds the initial form state for a new record. */
export function emptyRecord(fields: MetaField[]): Record_ {
  const out: Record_ = {};
  for (const f of editableFields(fields)) {
    out[f.name] = f.type === "bool" ? false : "";
  }
  return out;
}

/** submittable strips empty optional values so the server applies its own
 * defaults rather than storing an empty string where it expects null. */
export function submittable(fields: MetaField[], values: Record_): Record_ {
  const out: Record_ = {};
  for (const f of editableFields(fields)) {
    const v = values[f.name];
    if (v === "" && !f.required) {
      continue;
    }
    out[f.name] = v;
  }
  return out;
}

export type { FieldType, MetaField };

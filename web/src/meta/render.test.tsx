// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, test, vi } from "vitest";

import type { FieldType, MetaField } from "../api";
import {
  FieldControl,
  editableFields,
  emptyRecord,
  formatValue,
  labelFor,
  submittable,
} from "./render";

const field = (name: string, type: FieldType, required = false): MetaField => ({
  name,
  type,
  required,
});

const noFormat = {
  money: (minor: number, currency: string) => `${currency} ${minor}`,
  number: (x: number) => String(x),
};

// --- the type → control map ---

// The renderer must be total over the kernel's closed FieldType set: an
// unhandled type would mean a form field that silently cannot be filled in.
const ALL_TYPES: FieldType[] = [
  "text",
  "long_text",
  "rich_text",
  "int",
  "decimal",
  "money",
  "currency",
  "date",
  "datetime",
  "bool",
  "enum",
  "link",
  "table",
  "json",
  "file",
  "email",
  "phone",
  "address",
  "duration",
  "percent",
  "computed",
];

test("every kernel field type renders an editable control", () => {
  for (const type of ALL_TYPES) {
    const { container, unmount } = render(
      <FieldControl field={field("f", type)} id="f" value="" onChange={() => {}} />,
    );
    const control = container.querySelector("#f");
    expect(control, `no control rendered for type ${type}`).not.toBeNull();
    expect(
      ["INPUT", "TEXTAREA", "SELECT"].includes(control!.tagName),
      `type ${type} rendered a ${control!.tagName}, which is not a form control`,
    ).toBe(true);
    unmount();
  }
});

test("money, decimal and percent use text inputs, never number inputs", () => {
  // type="number" hands back a JS float; 0.07 would round-trip as
  // 0.07000000000000001. Exact decimal strings are the whole point (INV-F4).
  for (const type of ["money", "decimal", "percent"] as FieldType[]) {
    const { container, unmount } = render(
      <FieldControl field={field("amount", type)} id="amount" value="" onChange={() => {}} />,
    );
    const input = container.querySelector("#amount") as HTMLInputElement;
    expect(input.type, `${type} must not use a number input`).toBe("text");
    expect(input.inputMode).toBe("decimal");
    unmount();
  }
});

test("a money value round-trips as an exact string, never a number", async () => {
  const user = userEvent.setup();
  const seen: unknown[] = [];

  // A controlled wrapper, because that is how the form actually drives it —
  // testing the control with a frozen value would only prove it echoes one key.
  function Harness() {
    const [value, setValue] = useState<unknown>("");
    return (
      <FieldControl
        field={field("amount", "money")}
        id="amount"
        value={value}
        onChange={(v) => {
          seen.push(v);
          setValue(v);
        }}
      />
    );
  }

  render(<Harness />);
  // A value whose float round-trip is lossy: 0.07 * 100 is 7.000000000000001.
  await user.type(screen.getByRole("textbox"), "1234.07");

  expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("1234.07");
  expect(seen.at(-1)).toBe("1234.07");
  expect(seen.every((v) => typeof v === "string")).toBe(true);
});

test("date fields use the native date control", () => {
  const { container } = render(
    <FieldControl field={field("issue_date", "date")} id="d" value="2026-07-30T00:00:00Z" onChange={() => {}} />,
  );
  const input = container.querySelector("#d") as HTMLInputElement;
  expect(input.type).toBe("date");
  // A datetime from the server must not blank the native picker.
  expect(input.value).toBe("2026-07-30");
});

test("bool renders a checkbox reporting a boolean", async () => {
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(<FieldControl field={field("active", "bool")} id="a" value={false} onChange={onChange} />);

  await user.click(screen.getByRole("checkbox"));
  expect(onChange).toHaveBeenCalledWith(true);
});

test("computed fields are shown but not editable", () => {
  const { container } = render(
    <FieldControl field={field("total", "computed")} id="c" value="9" onChange={() => {}} />,
  );
  const input = container.querySelector("#c") as HTMLInputElement;
  expect(input.readOnly).toBe(true);
});

// --- display formatting ---

test("money is formatted from integer minor units with the record's currency", () => {
  const out = formatValue(field("gross_minor", "money"), 123456, { currency: "EUR" }, noFormat);
  expect(out).toBe("EUR 123456");
});

test("money with no currency shows raw minor units rather than guessing", () => {
  // Placing the decimal point without knowing the currency's exponent would be
  // wrong for JPY (0 digits) and TND (3) — showing the raw integer is honest.
  const out = formatValue(field("gross_minor", "money"), 123456, {}, noFormat);
  expect(out).toBe("123456");
});

test("empty values render as empty, not as 'null' or 'undefined'", () => {
  for (const v of [null, undefined, ""]) {
    expect(formatValue(field("name", "text"), v, {}, noFormat)).toBe("");
  }
});

// --- field selection ---

test("system columns and computed fields are never form inputs", () => {
  const fields = [
    field("id", "text"),
    field("tenant_id", "text"),
    field("created_at", "datetime"),
    field("name", "text"),
    field("total", "computed"),
  ];
  expect(editableFields(fields).map((f) => f.name)).toEqual(["name"]);
});

test("empty optional values are dropped so the server applies its defaults", () => {
  const fields = [field("name", "text", true), field("nickname", "text")];
  const out = submittable(fields, { name: "Acme", nickname: "" });
  expect(out).toEqual({ name: "Acme" });
});

test("empty required values are kept so the server validates them", () => {
  // Silently dropping a required-but-blank field would turn a validation error
  // into a confusing "field missing" one.
  const fields = [field("name", "text", true)];
  expect(submittable(fields, { name: "" })).toEqual({ name: "" });
});

test("a new record starts with typed empties", () => {
  const fields = [field("name", "text"), field("active", "bool")];
  expect(emptyRecord(fields)).toEqual({ name: "", active: false });
});

test("labels humanize snake_case field names", () => {
  expect(labelFor(field("issue_date", "date"))).toBe("Issue date");
  expect(labelFor(field("name", "text"))).toBe("Name");
});

// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen, within } from "@testing-library/react";
import { expect, test } from "vitest";

import type { MetaField } from "../api";
import { FieldControl, editableFields, groupFields, listFields, orderedFields } from "./render";

const f = (name: string, over: Partial<MetaField> = {}): MetaField => ({
  name,
  type: "text",
  required: false,
  ...over,
});

const label = (_key: string, fallback: string) => fallback;

// --- AC 4: the renderer drives field order from the schema ----------------

test("orderedFields sorts by the schema's order, not declaration order", () => {
  const fields = [f("c", { order: 3 }), f("a", { order: 1 }), f("b", { order: 2 })];
  expect(orderedFields(fields).map((x) => x.name)).toEqual(["a", "b", "c"]);
});

test("unset order keeps declaration order, so an undescribed schema is unchanged", () => {
  const fields = [f("first"), f("second"), f("third")];
  expect(orderedFields(fields).map((x) => x.name)).toEqual(["first", "second", "third"]);
});

test("ordered and unordered fields interleave by order, ties keeping declaration order", () => {
  // An overlay numbering a new field is how it slots between two core ones.
  const fields = [f("zero_a"), f("zero_b"), f("placed", { order: -1 })];
  expect(orderedFields(fields).map((x) => x.name)).toEqual(["placed", "zero_a", "zero_b"]);
});

test("editableFields and listFields inherit the schema's order", () => {
  const fields = [
    f("id"), // a system field: dropped
    f("last", { order: 2 }),
    f("first", { order: 1 }),
    f("derived", { type: "computed", order: 0 }), // computed: dropped from forms
  ];
  expect(editableFields(fields).map((x) => x.name)).toEqual(["first", "last"]);
  expect(listFields(fields, 1).map((x) => x.name)).toEqual(["first"]);
});

// --- grouping -------------------------------------------------------------

test("groupFields splits into sections, ungrouped first, order preserved within", () => {
  const groups = groupFields([
    f("code"),
    f("street", { group: "address" }),
    f("name"),
    f("city", { group: "address" }),
    f("vat", { group: "tax" }),
  ]);
  expect(groups.map((g) => g.name)).toEqual(["", "address", "tax"]);
  expect(groups[0].fields.map((x) => x.name)).toEqual(["code", "name"]);
  expect(groups[1].fields.map((x) => x.name)).toEqual(["street", "city"]);
});

test("a schema with no groups produces exactly one ungrouped section", () => {
  const groups = groupFields([f("a"), f("b")]);
  expect(groups).toHaveLength(1);
  expect(groups[0].name).toBe("");
});

// --- enum options ---------------------------------------------------------

const kind = f("kind", { type: "enum", options: ["customer", "vendor", "both"], required: true });

test("an enum renders a select offering exactly the schema's options", () => {
  render(<FieldControl field={kind} id="kind" value="" onChange={() => {}} />);
  const select = screen.getByRole("combobox") as HTMLSelectElement;

  const values = Array.from(select.options)
    .map((o) => o.value)
    .filter((v) => v !== "");
  expect(values).toEqual(["customer", "vendor", "both"]);
  // Nothing outside the closed set can be picked, which is the point: the
  // control now offers what the server will accept rather than free text.
  expect(values).not.toContain("banana");
  expect(select.required).toBe(true);
});

test("enum option labels translate, falling back to the humanized value", () => {
  render(
    <FieldControl
      field={kind}
      id="kind"
      object="Contact"
      label={(key, fallback) => (key === "schema.option.Contact.kind.customer" ? "Kunde" : fallback)}
      value=""
      onChange={() => {}}
    />,
  );
  const select = screen.getByRole("combobox");
  expect(within(select).getByText("Kunde")).toBeTruthy();
  // No key for "vendor": humanized, never a raw key.
  expect(within(select).getByText("Vendor")).toBeTruthy();
});

test("an enum from a server newer than this bundle degrades to text, not an empty select", () => {
  // A bundle can outlive its server's schema. An empty select is unusable;
  // a text box at least lets the user proceed.
  const { container } = render(
    <FieldControl field={f("status", { type: "enum" })} id="status" value="" onChange={() => {}} />,
  );
  expect((container.querySelector("#status") as HTMLElement).tagName).toBe("INPUT");
});

// --- widget overrides -----------------------------------------------------

test("widget textarea overrides a text field's single-line control", () => {
  const { container } = render(
    <FieldControl field={f("notes", { widget: "textarea" })} id="notes" value="" onChange={() => {}} />,
  );
  expect((container.querySelector("#notes") as HTMLElement).tagName).toBe("TEXTAREA");
});

test("widget radio renders a labelled radio group over the option set", () => {
  render(
    <FieldControl
      field={{ ...kind, widget: "radio" }}
      id="kind"
      object="Contact"
      label={label}
      value="vendor"
      onChange={() => {}}
    />,
  );
  const group = screen.getByRole("radiogroup");
  expect(group.getAttribute("aria-labelledby")).toBe("kind-label");

  const radios = within(group).getAllByRole("radio") as HTMLInputElement[];
  expect(radios.map((r) => r.value)).toEqual(["customer", "vendor", "both"]);
  expect(radios.find((r) => r.checked)?.value).toBe("vendor");
  // Each option is individually labelled — a radio group's outer label names
  // the question, not the answers.
  expect(within(group).getByLabelText("Customer")).toBeTruthy();
});

test("a widget the type does not support is ignored rather than breaking the control", () => {
  // The server refuses such a schema, so this only happens with a bundle older
  // or newer than its server. Falling back to the type's control keeps the form
  // usable either way.
  const { container } = render(
    <FieldControl
      field={f("count", { type: "int", widget: "radio" })}
      id="count"
      value=""
      onChange={() => {}}
    />,
  );
  expect((container.querySelector("#count") as HTMLInputElement).type).toBe("number");
});

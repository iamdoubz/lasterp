// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  ConformanceError,
  columnType,
  conform,
  createTableSQL,
  EXCLUDED_TYPES,
  replicableObjects,
  replicaFields,
  tableName,
  type MetaField,
  type MetaObject,
} from "./schema.ts";

function field(name: string, type: string, extra: Partial<MetaField> = {}): MetaField {
  return { name, type, required: false, ...extra };
}

function object(name: string, fields: MetaField[], persistence = "crud"): MetaObject {
  return { name, resource: `/api/v1/${name.toLowerCase()}`, module: "test", persistence, fields };
}

describe("replicable objects", () => {
  it("keeps crud objects and drops event-sourced ones", () => {
    const objects = [
      object("Contact", [field("name", "text")]),
      object("JournalEntry", [field("memo", "text")], "event_sourced"),
    ];
    expect(replicableObjects(objects).map((o) => o.name)).toEqual(["Contact"]);
  });
});

describe("excluded field types", () => {
  // WP-2.2b-decisions.md §3: the exclusion is asserted rather than left to a
  // switch's default branch, because an exclusion that happens by falling
  // through is one refactor away from becoming an inclusion.
  it("is exactly table, computed and file", () => {
    expect([...EXCLUDED_TYPES].sort()).toEqual(["computed", "file", "table"]);
  });

  it("keeps every excluded type out of the generated columns", () => {
    const o = object("Thing", [
      field("name", "text"),
      field("lines", "table", { target: "Line" }),
      field("total", "computed"),
      field("scan", "file"),
    ]);
    expect(replicaFields(o).map((f) => f.name)).toEqual(["name"]);

    const sql = createTableSQL(o);
    for (const excluded of ["lines", "total", "scan"]) {
      expect(sql).not.toContain(`"${excluded}"`);
    }
  });
});

describe("column types", () => {
  it("stores money, decimal and percent as one exact-string column", () => {
    // WP-2.2b-decisions.md §1 — this revises WP-2.2 §6's "two columns". The
    // server has always used one (ddl.go:32) and the AC compares row for row.
    for (const type of ["money", "decimal", "percent", "currency"]) {
      expect(columnType(type)).toBe("TEXT");
    }
    const sql = createTableSQL(object("Invoice", [field("total", "money")]));
    expect(sql).toContain(`"total" TEXT`);
    expect(sql).not.toContain("total_amount");
    expect(sql).not.toContain("total_currency");
  });

  it("declares TEXT for dates rather than a name SQLite gives NUMERIC affinity", () => {
    expect(columnType("date")).toBe("TEXT");
    expect(columnType("datetime")).toBe("TEXT");
  });

  it("maps int and bool to INTEGER", () => {
    expect(columnType("int")).toBe("INTEGER");
    expect(columnType("bool")).toBe("INTEGER");
  });
});

describe("generated DDL", () => {
  it("carries the kernel columns including tenant_id", () => {
    // tenant_id is the evidence for INV-T1: a foreign row must be detectably
    // absent, not merely absent.
    const sql = createTableSQL(object("Contact", [field("name", "text")]));
    expect(sql).toContain("obj_contact");
    for (const col of ["id TEXT PRIMARY KEY", "tenant_id", "created_at", "updated_at", "archived_at"]) {
      expect(sql).toContain(col);
    }
  });

  it("includes overlay-added fields as real columns", () => {
    // §2a: the server splits core columns from an overlay blob because one
    // table serves every tenant. A replica serves one, and /meta/objects does
    // not publish FromOverlay anyway.
    const sql = createTableSQL(object("Contact", [field("name", "text"), field("loyalty_tier", "text")]));
    expect(sql).toContain(`"loyalty_tier" TEXT`);
  });

  it("names tables the way the server does", () => {
    expect(tableName("JournalEntry")).toBe("obj_journalentry");
  });
});

describe("conform (INV-T5 at the replica boundary)", () => {
  it("passes null through for any type", () => {
    expect(conform("Contact", field("name", "text"), null)).toBeNull();
    expect(conform("Contact", field("age", "int"), undefined)).toBeNull();
  });

  it("refuses a numeric money value", () => {
    // The case this check exists for. JSON's only numeric type is a double:
    // 0.07 arrives as 0.07000000000000001 and minor units lose precision past
    // 2^53. Coercing it would make the replica disagree with the ledger.
    expect(() => conform("Invoice", field("total", "money"), 1999)).toThrow(ConformanceError);
    expect(conform("Invoice", field("total", "money"), '{"amount":1999,"currency":"USD"}')).toBe(
      '{"amount":1999,"currency":"USD"}',
    );
  });

  it("refuses an enum value outside its declared options", () => {
    const kind = field("kind", "enum", { options: ["customer", "supplier"] });
    expect(conform("Contact", kind, "customer")).toBe("customer");
    expect(() => conform("Contact", kind, "banana")).toThrow(ConformanceError);
  });

  it("refuses a non-integer int and a non-boolean bool", () => {
    expect(() => conform("Thing", field("n", "int"), 1.5)).toThrow(ConformanceError);
    expect(() => conform("Thing", field("n", "int"), "3")).toThrow(ConformanceError);
    expect(conform("Thing", field("ok", "bool"), true)).toBe(1);
    expect(conform("Thing", field("ok", "bool"), false)).toBe(0);
    expect(() => conform("Thing", field("ok", "bool"), 1)).toThrow(ConformanceError);
  });

  it("names the object and field so a failure is diagnosable", () => {
    expect(() => conform("Contact", field("kind", "enum", { options: ["a"] }), "b")).toThrow(
      /Contact\.kind/,
    );
  });
});

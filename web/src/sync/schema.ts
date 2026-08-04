// SPDX-License-Identifier: AGPL-3.0-only

// The replica's schema, generated from GET /api/v1/meta/objects.
//
// Never a hand-written mirror: CLAUDE.md forbids duplicating generated types,
// ADR-006 makes the effective schema per-tenant (so a static mirror would be
// wrong for any tenant with an overlay), and ADR-017 rejected Rust largely on
// the grounds that it would need a second generator. Hand-writing the DDL here
// would break the same rule in the same component.

import type { SqlValue } from "./port.ts";

/** One field as /meta/objects publishes it (internal/app/meta.go metaField). */
export interface MetaField {
  name: string;
  type: string;
  required: boolean;
  target?: string;
  localized?: boolean;
  options?: string[];
  order?: number;
  group?: string;
  widget?: string;
}

/** One object as /meta/objects publishes it (internal/app/meta.go metaObject). */
export interface MetaObject {
  name: string;
  resource: string;
  module: string;
  persistence: string;
  fields: MetaField[];
}

/** EXCLUDED field types never reach the replica, each for a stated reason
 * (WP-2.2-decisions.md §6, confirmed in WP-2.2b-decisions.md §3):
 *
 *   - `table`    a child-collection field is a second table with a parent link,
 *                and modelling child collections is its own design. Nothing in
 *                Phase 1 declares one.
 *   - `computed` derived on the server by definition; storing it client-side
 *                invents a second evaluator that can disagree.
 *   - `file`     a pointer to blob storage; offline blobs are Phase 4+.
 *
 * This is an explicit set rather than a `default:` fall-through on purpose. An
 * exclusion that happens by falling off the end of a switch is one refactor
 * away from silently becoming an inclusion, and the failure would be a replica
 * quietly carrying a column nobody decided to give it. */
export const EXCLUDED_TYPES: ReadonlySet<string> = new Set(["table", "computed", "file"]);

/** Objects with no CRUD surface cannot be hydrated: /api/v1/sync/snapshot 404s
 * for them. See WP-2.2b-decisions.md §2 — this is why `persistence` is on the
 * metadata payload at all. */
const PERSISTENCE_CRUD = "crud";

/** The kernel's own columns, present on every generated table. These mirror
 * `selectColumns`/`scanRecord` in kernel/metadata/crud.go, which is the shape
 * every record arrives in.
 *
 * `tenant_id` is carried even though a replica holds exactly one tenant. It is
 * the evidence for INV-T1: a row from another tenant is not merely absent, it
 * is *detectably* absent, and the convergence test asserts the column is
 * uniform. Dropping it would make the invariant unfalsifiable on the client. */
const KERNEL_COLUMNS = [
  "id TEXT PRIMARY KEY",
  "tenant_id TEXT NOT NULL",
  "created_at TEXT NOT NULL",
  "updated_at TEXT NOT NULL",
  "archived_at TEXT",
];

/** Field names the kernel reserves for the translation machinery. They arrive
 * on the record but are not columns (kernel/metadata/localized.go). */
const RESERVED_KEYS: ReadonlySet<string> = new Set(["translations", "i18n"]);

/** tableName mirrors kernel/metadata.TableName. */
export function tableName(object: string): string {
  return "obj_" + object.toLowerCase();
}

/**
 * columnType maps a field type to a SQLite column type.
 *
 * This mirrors the *value representation* of kernel/metadata/ddl.go
 * `columnType`, not its literal type names, because SQLite assigns affinity
 * from the name and Postgres does not. `TIMESTAMPTZ` is the clearest case: it
 * contains no affinity keyword, so SQLite would give it NUMERIC affinity and
 * store an ISO-8601 string only by falling back. Declaring TEXT says what is
 * actually stored.
 *
 * Money, decimal and percent are TEXT holding the exact string the server
 * stores — one column, not two. WP-2.2 §6 said two columns; the server has
 * always used one (ddl.go:32), and the AC is row-for-row equality against the
 * server's projection, so a different physical shape would put a translation
 * layer inside the oracle. Reasoning in WP-2.2b-decisions.md §1. No float
 * appears on either side, which is what the commandment actually requires.
 */
export function columnType(fieldType: string): string {
  switch (fieldType) {
    case "int":
      return "INTEGER";
    case "bool":
      // SQLite has no boolean; 0/1 with INTEGER affinity round-trips exactly.
      return "INTEGER";
    default:
      return "TEXT";
  }
}

/** replicableObjects filters /meta/objects down to what a replica can hold.
 *
 * Event-sourced objects are excluded here rather than discovered by a 404: a
 * replica of a stream is a different shape from a replica of a row, and
 * WP-2.2-decisions.md §9 defers it. */
export function replicableObjects(objects: MetaObject[]): MetaObject[] {
  return objects.filter((o) => o.persistence === PERSISTENCE_CRUD);
}

/** replicaFields returns the fields of an object that become columns.
 *
 * Every effective field becomes a real column, overlay ones included. The
 * server splits core columns from an overlay blob because one physical table
 * serves every tenant; a replica holds one tenant on one device and has no such
 * constraint. It is also the only shape /meta/objects supports — `metaField`
 * does not publish `FromOverlay`. See WP-2.2b-decisions.md §2a. */
export function replicaFields(object: MetaObject): MetaField[] {
  return object.fields.filter((f) => !EXCLUDED_TYPES.has(f.type) && !RESERVED_KEYS.has(f.name));
}

/** createTableSQL is the DDL for one object's replica table. */
export function createTableSQL(object: MetaObject): string {
  const columns = [...KERNEL_COLUMNS];
  for (const field of replicaFields(object)) {
    columns.push(`"${field.name}" ${columnType(field.type)}`);
  }
  return `CREATE TABLE IF NOT EXISTS ${tableName(object.name)} (\n  ${columns.join(",\n  ")}\n)`;
}

/** ConformanceError is an INV-T5 violation observed at the replica boundary:
 * a value arrived that does not fit its declared column.
 *
 * It is an error rather than a coercion on purpose. The client does not
 * re-validate business rules — the server is the referee (ADR-004) — but
 * silently rounding, truncating or stringifying a value that does not fit
 * would make the replica disagree with the server while looking healthy, which
 * is the one failure mode INV-S3 exists to catch. Surfacing it rolls back the
 * apply transaction and leaves the replica exactly where it was. */
export class ConformanceError extends Error {
  constructor(object: string, field: string, detail: string) {
    super(`sync: ${object}.${field} does not conform to its schema: ${detail}`);
    this.name = "ConformanceError";
  }
}

/**
 * conform checks one incoming value against its declared field type and returns
 * the value to bind, or throws ConformanceError.
 *
 * This is the client half of INV-T5 ("every stored field value conforms to its
 * object's effective schema — declared type and declared option set"). The
 * server enforces it on write; this enforces it on arrival, which is a
 * different guarantee: it catches a server that regressed, a payload mangled in
 * transit, and — the case worth the code — a float where money should be an
 * exact string.
 */
export function conform(object: string, field: MetaField, value: unknown): SqlValue {
  if (value === null || value === undefined) return null;

  switch (field.type) {
    case "int":
      if (typeof value !== "number" || !Number.isInteger(value)) {
        throw new ConformanceError(object, field.name, `want an integer, got ${describe(value)}`);
      }
      return value;

    case "bool":
      if (typeof value !== "boolean") {
        throw new ConformanceError(object, field.name, `want a boolean, got ${describe(value)}`);
      }
      return value ? 1 : 0;

    case "money":
    case "decimal":
    case "percent":
      // A number here is the bug this check exists for. JSON has one numeric
      // type and it is a double: 0.07 arrives as 0.07000000000000001 and a
      // large minor-unit amount silently loses precision past 2^53. The server
      // sends these as exact strings (ddl.go:32) and anything else is a
      // regression that must not reach storage.
      if (typeof value !== "string") {
        throw new ConformanceError(
          object,
          field.name,
          `want an exact string, got ${describe(value)} — a numeric ${field.type} loses precision`,
        );
      }
      return value;

    case "enum": {
      if (typeof value !== "string") {
        throw new ConformanceError(object, field.name, `want a string, got ${describe(value)}`);
      }
      // The declared option set is a value domain INV-T3 says no layer may
      // widen, and INV-T5 says no write may escape. An out-of-set value in the
      // replica would be a chart of accounts with a type nothing can classify.
      if (field.options && field.options.length > 0 && !field.options.includes(value)) {
        throw new ConformanceError(
          object,
          field.name,
          `${JSON.stringify(value)} is not one of ${JSON.stringify(field.options)}`,
        );
      }
      return value;
    }

    default:
      if (typeof value !== "string") {
        throw new ConformanceError(object, field.name, `want a string, got ${describe(value)}`);
      }
      return value;
  }
}

function describe(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  return `${typeof value} ${JSON.stringify(value)}`;
}

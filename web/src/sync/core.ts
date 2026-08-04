// SPDX-License-Identifier: AGPL-3.0-only

// The sync core: fold the server's change feed into a local replica, in order,
// exactly once, advancing a cursor that never runs ahead of what is committed.
//
// WP-2.6 built this as a spike against a single stand-in table to answer "can
// one core in this language serve every shell" (ADR-017). WP-2.2b is the real
// one: the same ordering, resume and crash-safety properties, now writing the
// tables generated from metadata in schema.ts.

import {
  conform,
  createTableSQL,
  replicableObjects,
  replicaFields,
  tableName,
  type MetaObject,
} from "./schema.ts";
import type { SqlValue, Store } from "./port.ts";

/** One entry of the server's change feed, as GET /api/v1/sync/changes returns.
 * A pointer: it says *what* changed, never what it now is. */
export interface Change {
  cursor: number;
  source: string;
  ref_id: string;
  object: string;
  scope_key: string;
  recorded_at: string;
}

/** A record as the CRUD engine returns it (kernel/metadata/crud.go scanRecord):
 * the declared fields plus the kernel's own columns. Values are unknown because
 * the shape is metadata-driven — `conform` resolves each against its field. */
export type ServerRecord = Record<string, unknown>;

/** One page of GET /api/v1/sync/changes?include=rows.
 *
 * `data` carries the ordered pointers; `rows` carries current row state for the
 * rows those pointers name, grouped by object. The two are separate because the
 * feed on disk is a pointer log and materialisation is a read-time join
 * (WP-2.1-decisions.md §3) — not because they describe different things. */
export interface FeedPage {
  data: Change[];
  cursor: number;
  rows?: Record<string, ServerRecord[]>;
}

/** SchemaIndex resolves an object name to its effective schema. */
export type SchemaIndex = Map<string, MetaObject>;

export function indexSchema(objects: MetaObject[]): SchemaIndex {
  return new Map(replicableObjects(objects).map((o) => [o.name, o]));
}

/** The replica's bookkeeping.
 *
 * `_sync_state` is docs/04's cursor store, keyed by scope from the start so
 * WP-2.4 making scopes plural is additive rather than a migration.
 *
 * `_hydration` tracks per-object snapshot progress. It is separate because
 * hydration pages per object while the feed cursor is one position for the
 * whole scope: a client interrupted halfway through hydrating must resume that
 * object, not restart the scope (WP-2.2b-decisions.md §4). */
const BOOKKEEPING = [
  `CREATE TABLE IF NOT EXISTS _sync_state (
     scope TEXT PRIMARY KEY,
     cursor INTEGER NOT NULL
   )`,
  `CREATE TABLE IF NOT EXISTS _hydration (
     object TEXT PRIMARY KEY,
     after_id TEXT NOT NULL DEFAULT '',
     done INTEGER NOT NULL DEFAULT 0
   )`,

  // WP-2.3b, docs/04 §Replica: "_outbox (pending commands) … _conflicts".
  //
  // The DDL lives here rather than in outbox.ts so the dependency runs one way
  // (outbox.ts needs applyRecord; this file needs nothing back) and so that
  // "what a replica is made of" stays readable in one place.
  //
  // `seq` orders the drain. A command_id is a UUIDv7 and sorts chronologically,
  // but two minted in the same millisecond order by their random bits, and a
  // PATCH ahead of the POST that creates its row is a 404 the drain would file
  // as a rejection — a conflict fabricated by the sort (WP-2.3-decisions.md
  // §12). `command_id` is unique because it is also the Idempotency-Key, and
  // queueing one twice would mean two commands sharing an outcome.
  //
  // `before` is the pre-image for rollback. A rejected *update* has nothing to
  // recover from otherwise: server state never changed, so no amount of pulling
  // repairs the local edit.
  `CREATE TABLE IF NOT EXISTS _outbox (
     seq INTEGER PRIMARY KEY AUTOINCREMENT,
     command_id TEXT NOT NULL UNIQUE,
     method TEXT NOT NULL,
     path TEXT NOT NULL,
     body TEXT,
     object TEXT NOT NULL,
     row_id TEXT NOT NULL,
     before TEXT,
     attempts INTEGER NOT NULL DEFAULT 0,
     created_at TEXT NOT NULL
   )`,

  // The `pending` flag docs/04 §Upstream 1 describes, as a sidecar rather than
  // a column on every generated table (WP-2.3-decisions.md §3): a column would
  // have to be excluded from the INV-S3 convergence comparison, which is the
  // translation layer WP-2.2b-decisions.md §1 kept out of the oracle on purpose.
  // A sidecar is invisible to it, and outlives the row being deleted.
  `CREATE TABLE IF NOT EXISTS _pending (
     object TEXT NOT NULL,
     row_id TEXT NOT NULL,
     command_id TEXT NOT NULL,
     PRIMARY KEY (object, row_id, command_id)
   )`,

  // The tray. Columns are the server's own problem+json, because the point of
  // replaying an ordinary request is that the rejection is already a rendered,
  // localized explanation — the client shows what the server said rather than a
  // translation of it (WP-2.3-decisions.md §1).
  `CREATE TABLE IF NOT EXISTS _conflicts (
     command_id TEXT PRIMARY KEY,
     method TEXT NOT NULL,
     path TEXT NOT NULL,
     object TEXT NOT NULL,
     row_id TEXT NOT NULL,
     body TEXT,
     status INTEGER NOT NULL,
     type TEXT NOT NULL,
     title TEXT NOT NULL,
     detail TEXT NOT NULL,
     filed_at TEXT NOT NULL
   )`,

  // The schema this replica was generated from, kept so it can be opened with
  // no network. Until WP-2.3b that was not needed: a read-only replica with no
  // server is a stale read, and `open` could simply require /meta/objects. An
  // outbox changes it — a tab reloaded offline must still be able to accept a
  // write, and it cannot resolve one without knowing its objects' fields.
  //
  // It is a cache and never a source: a successful fetch overwrites it, so
  // ADR-006's "the effective schema is per-tenant and can change" is unaffected.
  `CREATE TABLE IF NOT EXISTS _meta (
     id INTEGER PRIMARY KEY CHECK (id = 1),
     objects TEXT NOT NULL
   )`,
];

/** The single scope this WP tracks. WP-2.4 makes scopes plural. */
export const SCOPE = "default";

/** initBookkeeping creates the replica's own tables — everything that is not
 * generated from metadata. Split out because the metadata cache has to be
 * readable *before* there is a schema to generate from: a client opening
 * offline reads its objects out of this. */
export function initBookkeeping(store: Store): void {
  for (const stmt of BOOKKEEPING) store.exec(stmt);
  store.exec(`INSERT OR IGNORE INTO _sync_state (scope, cursor) VALUES (?, 0)`, [SCOPE]);
}

/** initReplica creates the bookkeeping tables and one table per replicable
 * object. Idempotent: opening an existing replica is the same call. */
export function initReplica(store: Store, objects: MetaObject[]): void {
  store.transaction(() => {
    initBookkeeping(store);
    for (const object of replicableObjects(objects)) {
      store.exec(createTableSQL(object));
      store.exec(`INSERT OR IGNORE INTO _hydration (object) VALUES (?)`, [object.name]);
    }
  });
}

/** cacheMeta records the schema this replica was generated from. */
export function cacheMeta(store: Store, objects: MetaObject[]): void {
  store.exec(
    `INSERT INTO _meta (id, objects) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET objects = excluded.objects`,
    [JSON.stringify(objects)],
  );
}

/** cachedMeta returns the last schema fetched, or null when this replica has
 * never reached the server. */
export function cachedMeta(store: Store): MetaObject[] | null {
  initBookkeeping(store);
  const rows = store.query<{ objects: string }>(`SELECT objects FROM _meta WHERE id = 1`);
  if (rows.length === 0) return null;
  const parsed: unknown = JSON.parse(String(rows[0].objects));
  return Array.isArray(parsed) ? (parsed as MetaObject[]) : null;
}

export function currentCursor(store: Store): number {
  const rows = store.query<{ cursor: number }>(`SELECT cursor FROM _sync_state WHERE scope = ?`, [
    SCOPE,
  ]);
  return rows.length > 0 ? Number(rows[0].cursor) : 0;
}

/**
 * applyPage folds one page of the feed into the replica and returns the new
 * cursor.
 *
 * Four properties, all load-bearing and all tested:
 *
 *   - **Ordered.** Pointers apply in cursor order. A page that arrives out of
 *     order is a bug upstream, so it is rejected rather than sorted into place
 *     — silently repairing it would hide the very defect INV-S5 exists to
 *     prevent.
 *   - **Exactly once.** Entries at or below the current cursor do not move the
 *     cursor, so a client that crashes after committing but before
 *     acknowledging can safely re-request the same page.
 *   - **Crash-safe.** Rows and cursor move in one transaction. If this throws
 *     mid-page, the replica is exactly where it started — including on a
 *     conformance failure, which is why INV-T5 can be an error rather than a
 *     coercion.
 *   - **Idempotent.** Row state is current server state (WP-2.2-decisions.md
 *     §2), so applying it twice lands the same value. That is what lets the
 *     replica hold state newer than its own cursor claims: ahead, never behind.
 */
export function applyPage(store: Store, index: SchemaIndex, page: FeedPage): number {
  return store.transaction(() => {
    // Two distinct comparisons, and collapsing them into one is a bug the first
    // version of this file had. `start` decides what has already been applied —
    // a client re-requesting from an older cursor legitimately re-sends a
    // prefix. `previous` decides whether the page itself ascends.
    //
    // With a single running cursor for both, a descending page ([3, 2]) is
    // indistinguishable from an overlapping one: 3 applies, then 2 looks
    // "already applied" and is silently dropped. The replica quietly loses an
    // entry the server sent — an INV-S3 divergence, detected nowhere.
    const start = currentCursor(store);
    let cursor = start;
    let previous = -1;

    for (const change of page.data) {
      if (change.cursor <= previous) {
        throw new Error(
          `sync: feed page does not ascend: cursor ${change.cursor} after ${previous}`,
        );
      }
      previous = change.cursor;
      if (change.cursor > start) cursor = change.cursor;
    }

    for (const [object, records] of Object.entries(page.rows ?? {})) {
      const schema = index.get(object);
      if (schema === undefined) {
        // The server materialised rows for something this replica has no table
        // for. Skipping would drop data permanently and look healthy, which is
        // exactly the divergence no amount of syncing repairs (§5). Event-
        // sourced pointers never appear here — materialize() only resolves
        // audit-sourced entries — so this is an anomaly, not a normal case.
        throw new Error(`sync: feed carried rows for unknown object ${JSON.stringify(object)}`);
      }
      for (const record of records) applyRecord(store, schema, record);
    }

    // The server's own resume position. Until WP-2.4 the cursor could be
    // derived entirely from the entries in the page, and deriving it was the
    // conservative choice. A scope filter ends that: the entries this page's
    // cursor covers may have been filtered out before it was sent, so there is
    // nothing local to derive from and the server's number is the only one that
    // knows what was skipped (WP-2.4-decisions.md §7).
    //
    // Ahead is taken, behind is fatal. A page reporting a resume point *below*
    // an entry it just delivered would strand every entry in between on the
    // next request — the exact failure INV-S5 exists to prevent — so it fails
    // loudly here rather than being quietly clamped.
    if (page.cursor < previous) {
      throw new Error(
        `sync: feed page resumes at ${page.cursor}, behind entry ${previous} it delivered`,
      );
    }
    if (page.cursor > cursor) cursor = page.cursor;

    store.exec(`UPDATE _sync_state SET cursor = ? WHERE scope = ?`, [cursor, SCOPE]);
    return cursor;
  });
}

/** applyRecord upserts one row, or deletes it when the server says it is
 * archived.
 *
 * Deletes must be conveyed rather than omitted: `CRUD.List` filters archived
 * rows, which is right for a UI and wrong for a replica — a client that already
 * holds a row and is told nothing keeps it forever (WP-2.2-decisions.md §5). So
 * materialisation returns them flagged and the apply loop removes them here. */
export function applyRecord(store: Store, schema: MetaObject, record: ServerRecord): void {
  const id = record["id"];
  if (typeof id !== "string" || id === "") {
    throw new Error(`sync: ${schema.name} record has no id`);
  }

  if (record["archived_at"] != null) {
    store.exec(`DELETE FROM ${tableName(schema.name)} WHERE id = ?`, [id]);
    return;
  }

  const columns = ["id", "tenant_id", "created_at", "updated_at", "archived_at"];
  const values: SqlValue[] = [
    id,
    text(schema.name, "tenant_id", record["tenant_id"]),
    text(schema.name, "created_at", record["created_at"]),
    text(schema.name, "updated_at", record["updated_at"]),
    null,
  ];

  for (const field of replicaFields(schema)) {
    columns.push(field.name);
    values.push(conform(schema.name, field, record[field.name]));
  }

  const placeholders = columns.map(() => "?").join(", ");
  const quoted = columns.map((c) => `"${c}"`).join(", ");
  const updates = columns
    .filter((c) => c !== "id")
    .map((c) => `"${c}" = excluded."${c}"`)
    .join(", ");

  store.exec(
    `INSERT INTO ${tableName(schema.name)} (${quoted}) VALUES (${placeholders})
     ON CONFLICT (id) DO UPDATE SET ${updates}`,
    values,
  );
}

/** text reads a kernel column, which is always a present string. These are not
 * declared fields so they have no MetaField to conform against, but a missing
 * one is still a malformed payload rather than something to store as null. */
function text(object: string, column: string, value: unknown): string {
  if (typeof value !== "string" || value === "") {
    throw new Error(`sync: ${object}.${column} is missing from the server record`);
  }
  return value;
}

/** rowCount is a convenience for tests and the bench. */
export function rowCount(store: Store, object: string): number {
  const rows = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM ${tableName(object)}`);
  return Number(rows[0].n);
}

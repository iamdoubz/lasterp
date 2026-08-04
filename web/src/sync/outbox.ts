// SPDX-License-Identifier: AGPL-3.0-only

// The outbox: offline writes, applied optimistically and replayed on reconnect.
//
// docs/04 §Upstream, ADR-004 §Decision 2. The shape is decided in
// WP-2.3-decisions.md and the one sentence that explains the rest is §1: **a
// command is a stored HTTP request, and the drain is a replay of it**. There is
// no sync write endpoint. The drain issues the same `POST /api/v1/contact` the
// online UI issues, through the same client, with the command_id as the
// Idempotency-Key.
//
// That is why this file is as small as it is. Exactly-once comes from the
// gateway's idempotency store (INV-E4), accept-with-rebase is the response body
// applied to the replica, and a rejection is the server's own problem+json in
// the tray. None of the three needed a protocol.

import { applyRecord, type ServerRecord, type SchemaIndex } from "./core.ts";
import { conform, replicaFields, tableName, type MetaObject } from "./schema.ts";
import type { CommandResult, Transport } from "./transport.ts";
import type { SqlValue, Store } from "./port.ts";

/** UNPERSISTED_OUTBOX_LIMIT caps pending commands when
 * `navigator.storage.persist()` was **denied** (WP-2.3-decisions.md §6).
 *
 * Refusing offline writes outright is the strictest reading of no-silent-loss
 * and the wrong trade: Chrome denies persistence to sites without engagement,
 * so the strict rule leaves an ordinary tab with no offline capability at all —
 * a certain loss of function to avoid a probabilistic loss of data. The cap
 * bounds the blast radius of an eviction instead, and the count is shown to the
 * user from the first write rather than sprung on them at the limit.
 *
 * When persistence is *granted* there is no cap: a replica that cannot be
 * evicted has no radius to bound.
 *
 * Fifty is a session's worth of offline edits. It is not derived from anything,
 * because nothing to derive it from exists yet — what makes it honest is that
 * it is named, asserted by a test, and displayed. */
export const UNPERSISTED_OUTBOX_LIMIT = 50;

/** MAX_ATTEMPTS bounds retrying one command.
 *
 * Retryable failures (a 5xx, a still-in-flight idempotency key — §11) leave the
 * command queued, which is right until it is not: a command that will never
 * succeed would otherwise sit at the head of a strictly-ordered queue forever,
 * blocking everything behind it. That is not a silent drop, but it is a silent
 * stall, and INV-S4's "surfaced to the user" covers both. Past this it is filed
 * as a conflict like any other. */
export const MAX_ATTEMPTS = 8;

/** OutboxFullError is the unpersisted cap being reached. It is a distinct type
 * because the shell renders it as a state — "unsent work is at its limit, go
 * online" — rather than as a failed save. An opaque failure is the one outcome
 * §6 rules out. */
export class OutboxFullError extends Error {
  // A plain field, not a constructor parameter property: the convergence driver
  // runs under Node's strip-only TypeScript mode, which erases types and
  // refuses any syntax that would need emitting code (ERR_UNSUPPORTED_TYPESCRIPT
  // _SYNTAX). ADR-017 chose "no build step between the source under test and the
  // thing being run", and this is the shape of that cost.
  readonly limit: number;

  constructor(limit: number) {
    super(`sync: ${limit} unsent commands and this browser has not granted persistent storage`);
    this.name = "OutboxFullError";
    this.limit = limit;
  }
}

/** OutboxParseError is a stored command that does not parse.
 *
 * `Store.query` returns `T[]` by an unchecked cast, which is right for
 * replicated rows — they are overwritten from the server, so a malformed one
 * repairs itself. It is wrong here. The outbox is the first thing in this
 * client the server cannot reconstruct (WP-2.2b-decisions.md §7), so a row that
 * does not parse is *work that is gone*, and casting means finding that out at
 * the point of use rather than at the boundary. */
export class OutboxParseError extends Error {
  constructor(detail: string) {
    super(`sync: unreadable outbox row: ${detail}`);
    this.name = "OutboxParseError";
  }
}

/** The write verbs an offline command can carry. Closed: these are the generic
 * CRUD routes, and a lifecycle verb (post an invoice, close a period) is
 * online-required by default anyway (docs/04 §Offline capability matrix). */
export type CommandMethod = "POST" | "PATCH" | "DELETE";

/** One command as the caller submits it. */
export interface Command {
  /** UUIDv7, and *also* the Idempotency-Key. One identifier, so a replay is
   * deduped by the machinery that already exists (INV-E4). */
  commandId: string;
  method: CommandMethod;
  /** The object's name, for the schema this command's row belongs to. */
  object: string;
  /** The row this command creates, changes or removes. For a create the client
   * chooses it, and the server honours it (§2) — so the row the user is looking
   * at offline is the row the server ends up with. */
  rowId: string;
  /** The request body, or null for a delete. */
  body: Record<string, unknown> | null;
}

/** One row of `_outbox`, parsed. */
export interface OutboxCommand extends Command {
  seq: number;
  path: string;
  /** The row as it was before this command was applied optimistically, or null
   * when the command created it. This is the rollback: docs/04 §Upstream 3 says
   * a rejected command rolls its optimistic rows back, and for an *update*
   * there is no other way to do it — the server state never changed, so no
   * amount of pulling will repair the local edit. */
  before: ServerRecord | null;
  attempts: number;
}

/** One filed rejection. The fields are the server's own problem+json: the tray
 * renders what the server said rather than a translation of it (§1). */
export interface Conflict {
  commandId: string;
  method: CommandMethod;
  path: string;
  object: string;
  rowId: string;
  body: Record<string, unknown> | null;
  status: number;
  type: string;
  title: string;
  detail: string;
  filedAt: string;
}

/** What one drain did. Every command it took off the queue is counted in
 * exactly one of the first two, which is the conservation INV-S4 asserts. */
export interface DrainReport {
  accepted: number;
  conflicted: number;
  /** Still queued when the drain stopped: a partition, a retryable failure, or
   * everything behind a rejection (stop-on-reject, §9). */
  pending: number;
}

/** commandPath builds the route this command replays into.
 *
 * `resource` comes from `/meta/objects` (`api.ResourcePath`) rather than being
 * re-derived from the object name — the gateway routes on it, and a client that
 * lowercases the name itself is one renaming away from posting into a 404. */
export function commandPath(schema: MetaObject, method: CommandMethod, rowId: string): string {
  const base = `/api/v1/${schema.resource}`;
  return method === "POST" ? base : `${base}/${rowId}`;
}

/** enqueue stores a command and applies it to the replica optimistically.
 *
 * Both halves are one transaction. A command stored without its optimistic row
 * is work the user cannot see; an optimistic row without its command is work
 * that will never be sent and never be missed — which is the silent loss INV-S1
 * names. Neither is allowed to exist alone, so neither is written alone. */
export function enqueue(
  store: Store,
  index: SchemaIndex,
  command: Command,
  persisted: boolean,
): void {
  const schema = index.get(command.object);
  if (schema === undefined) {
    throw new Error(`sync: cannot queue a command for unknown object ${JSON.stringify(command.object)}`);
  }

  store.transaction(() => {
    if (!persisted && outboxDepth(store) >= UNPERSISTED_OUTBOX_LIMIT) {
      throw new OutboxFullError(UNPERSISTED_OUTBOX_LIMIT);
    }

    const before = rowSnapshot(store, schema, command.rowId);
    applyOptimistically(store, index, schema, command, before);

    store.exec(
      `INSERT INTO _outbox (command_id, method, path, body, object, row_id, before, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      [
        command.commandId,
        command.method,
        commandPath(schema, command.method, command.rowId),
        command.body === null ? null : JSON.stringify(command.body),
        command.object,
        command.rowId,
        before === null ? null : JSON.stringify(before),
        new Date().toISOString(),
      ],
    );
    store.exec(
      `INSERT OR IGNORE INTO _pending (object, row_id, command_id) VALUES (?, ?, ?)`,
      [command.object, command.rowId, command.commandId],
    );
  });
}

/**
 * drain replays queued commands, in order, until the queue is empty or
 * something stops it.
 *
 * Three outcomes per command, and the whole upstream design is which is which:
 *
 *   - **2xx** — accepted. The response body is current server state for that
 *     row, so applying it *is* accept-with-rebase: server-assigned values land
 *     without a rebase protocol to carry them (ADR-004 §Upstream 3).
 *   - **retryable** — a 5xx, a rate limit, or an idempotency key whose original
 *     request is still in flight (§11). Left queued, and the drain stops:
 *     order is per-client causal, so running the next command past a stuck one
 *     is not a choice this can make.
 *   - **anything else** — rejected. The optimistic rows roll back, the server's
 *     problem+json goes to the tray, and the drain stops there
 *     (stop-on-reject, §9): no command runs whose predecessor may have been its
 *     precondition.
 *
 * A transport failure is not an outcome at all — it means the request may or
 * may not have been received, so the command stays exactly where it is and the
 * next drain re-sends it. That re-send is safe because the command_id is the
 * Idempotency-Key (INV-E4), which is the whole reason RPO 0 (INV-S1) is
 * achievable across a crash.
 */
export async function drain(
  store: Store,
  index: SchemaIndex,
  transport: Transport,
): Promise<DrainReport> {
  let accepted = 0;
  let conflicted = 0;

  for (;;) {
    const command = nextCommand(store);
    if (command === undefined) break;

    const schema = index.get(command.object);
    if (schema === undefined) {
      // The object left this replica's metadata while a command for it was
      // queued — a module was disabled. Sending it would 404 and land in a tray
      // the user cannot act on either, so file it with a client-side reason
      // rather than inventing a server one.
      store.transaction(() => {
        file(store, command, {
          status: 0,
          type: "about:blank",
          title: "object is no longer available",
          detail: `${command.object} is not part of this replica any more, so its queued change cannot be sent`,
        });
        clear(store, command);
      });
      conflicted++;
      break;
    }

    let result: CommandResult;
    try {
      result = await transport.command(command);
    } catch {
      // Offline, or the wire went down mid-request. Leave everything queued.
      break;
    }

    if (result.status >= 200 && result.status < 300) {
      try {
        store.transaction(() => {
          if (isRecord(result.body)) applyRecord(store, schema, result.body);
          clear(store, command);
        });
      } catch (err) {
        // Applying the server's own answer failed — a schema disagreement
        // (INV-T5), most plausibly a replica opened against cached metadata
        // that has since narrowed. The command itself *succeeded*, so leaving
        // it queued would re-send an applied write on every reconnect forever:
        // a replay returns the same 2xx, which never reaches the retry cap, so
        // nothing would ever surface it. That is the silent stall §11 exists to
        // prevent, reached down the one path that skips it.
        //
        // So acknowledge it — the server has the work — and let the failure
        // propagate loudly. The row arrives again through the feed, where the
        // same conformance check runs and fails the sync where it belongs.
        store.transaction(() => clear(store, command));
        throw err;
      }
      accepted++;
      continue;
    }

    if (retryable(result) && bumpAttempts(store, command) < MAX_ATTEMPTS) break;

    store.transaction(() => {
      rollback(store, schema, command);
      file(store, command, {
        status: result.status,
        type: problemField(result.body, "type", "about:blank"),
        title: problemField(result.body, "title", `the server refused this change (${result.status})`),
        detail: problemField(result.body, "detail", ""),
      });
      clear(store, command);
    });
    conflicted++;
    break;
  }

  return { accepted, conflicted, pending: outboxDepth(store) };
}

/** retryable says whether this response means "not now" rather than "no".
 *
 * The 409 is the one worth the comment. The gateway raises it both for a key
 * reused with a different request and for a key whose original request has not
 * finished — and the second is the ordinary consequence of the crash the outbox
 * exists to survive. Filing it as a rejection would show the user a conflict
 * for a command that is at that moment succeeding, which is INV-S4's failure
 * mode reached by trying to satisfy it. Hence the typed problem
 * (`api.ProblemIdempotencyConflict`) and this branch. */
function retryable(result: CommandResult): boolean {
  if (result.status >= 500) return true;
  if (result.status === 408 || result.status === 429) return true;
  return result.status === 409 && problemField(result.body, "type", "") === "idempotency-conflict";
}

// --- the queue ---

/** pendingCommands returns everything queued, oldest first.
 *
 * Ordered by `seq`, not by `command_id`. A UUIDv7 sorts chronologically, so
 * ordering by it is nearly right — but two commands minted in the same
 * millisecond order by their random bits, and a PATCH sorted ahead of the POST
 * that creates its row is a 404 the drain would file as a conflict. A
 * fabricated rejection caused by how the queue was sorted (§12). */
export function pendingCommands(store: Store): OutboxCommand[] {
  return store
    .query<unknown>(
      `SELECT seq, command_id, method, path, body, object, row_id, before, attempts
         FROM _outbox ORDER BY seq`,
    )
    .map(parseOutboxRow);
}

export function outboxDepth(store: Store): number {
  const rows = store.query<{ n: number }>(`SELECT COUNT(*) AS n FROM _outbox`);
  return Number(rows[0].n);
}

/** pendingRows lists the rows currently carrying an unsent change, so a UI can
 * mark them (docs/04 §Upstream 1: rows are flagged `pending`).
 *
 * The flag is this sidecar table rather than a column on every generated table,
 * for the INV-S3 oracle's sake: WP-2.2b-decisions.md §1 mirrored the server's
 * physical column shape exactly so convergence is a direct equality with no
 * translation inside it, and a `_pending` column would have to be excluded from
 * that comparison — putting the layer §1 rejected back into the oracle. A
 * sidecar is invisible to it, and survives its row being deleted (§3). */
export function pendingRows(store: Store, object: string): string[] {
  return store
    .query<{ row_id: string }>(`SELECT row_id FROM _pending WHERE object = ? ORDER BY row_id`, [
      object,
    ])
    .map((r) => String(r.row_id));
}

function nextCommand(store: Store): OutboxCommand | undefined {
  const rows = store.query<unknown>(
    `SELECT seq, command_id, method, path, body, object, row_id, before, attempts
       FROM _outbox ORDER BY seq LIMIT 1`,
  );
  return rows.length === 0 ? undefined : parseOutboxRow(rows[0]);
}

function bumpAttempts(store: Store, command: OutboxCommand): number {
  const attempts = command.attempts + 1;
  store.exec(`UPDATE _outbox SET attempts = ? WHERE seq = ?`, [attempts, command.seq]);
  return attempts;
}

function clear(store: Store, command: OutboxCommand): void {
  store.exec(`DELETE FROM _outbox WHERE seq = ?`, [command.seq]);
  store.exec(`DELETE FROM _pending WHERE command_id = ?`, [command.commandId]);
}

/**
 * parseOutboxRow validates one stored row and returns it typed, or throws.
 *
 * This is the port boundary the roadmap names. Everything else in this client
 * can be casually cast because the server is the source of truth for it; the
 * outbox is the exception, and a row that silently parsed to `undefined` fields
 * would be sent as a malformed request or skipped entirely — a lost write
 * wearing the costume of a successful drain.
 */
export function parseOutboxRow(raw: unknown): OutboxCommand {
  if (typeof raw !== "object" || raw === null) {
    throw new OutboxParseError(`want an object, got ${typeof raw}`);
  }
  const row = raw as Record<string, unknown>;

  const method = text(row, "method");
  if (method !== "POST" && method !== "PATCH" && method !== "DELETE") {
    throw new OutboxParseError(`method ${JSON.stringify(method)} is not a write verb`);
  }

  return {
    seq: integer(row, "seq"),
    commandId: text(row, "command_id"),
    method,
    path: text(row, "path"),
    object: text(row, "object"),
    rowId: text(row, "row_id"),
    body: json(row, "body"),
    before: json(row, "before"),
    attempts: integer(row, "attempts"),
  };
}

function text(row: Record<string, unknown>, key: string): string {
  const value = row[key];
  if (typeof value !== "string" || value === "") {
    throw new OutboxParseError(`${key} is ${describe(value)}, want a non-empty string`);
  }
  return value;
}

function integer(row: Record<string, unknown>, key: string): number {
  const value = Number(row[key]);
  if (!Number.isInteger(value)) {
    throw new OutboxParseError(`${key} is ${describe(row[key])}, want an integer`);
  }
  return value;
}

/** json parses a nullable JSON column. A column that holds unparseable text is
 * a throw, not a null: `null` means "this command has no body", and quietly
 * turning corruption into that would send a DELETE-shaped request for what was
 * a create. */
function json(row: Record<string, unknown>, key: string): Record<string, unknown> | null {
  const value = row[key];
  if (value === null || value === undefined) return null;
  if (typeof value !== "string") {
    throw new OutboxParseError(`${key} is ${describe(value)}, want JSON text or null`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch (err) {
    throw new OutboxParseError(`${key} is not JSON: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new OutboxParseError(`${key} parsed to ${describe(parsed)}, want an object`);
  }
  return parsed as Record<string, unknown>;
}

function describe(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  return `${typeof value} ${JSON.stringify(value)}`;
}

// --- conflicts ---

export function conflicts(store: Store): Conflict[] {
  return store
    .query<Record<string, SqlValue>>(
      `SELECT command_id, method, path, object, row_id, body, status, type, title, detail, filed_at
         FROM _conflicts ORDER BY filed_at, command_id`,
    )
    .map((row) => ({
      commandId: String(row.command_id),
      method: String(row.method) as CommandMethod,
      path: String(row.path),
      object: String(row.object),
      rowId: String(row.row_id),
      body: row.body === null ? null : (JSON.parse(String(row.body)) as Record<string, unknown>),
      status: Number(row.status),
      type: String(row.type),
      title: String(row.title),
      detail: String(row.detail),
      filedAt: String(row.filed_at),
    }));
}

/** discardConflict is the user deciding this change is not worth keeping. It is
 * the *only* way a command leaves the system without reaching the server, which
 * is what "no silent drops" means: something can be dropped, but only loudly
 * and only by the person whose work it is. */
export function discardConflict(store: Store, commandId: string): void {
  store.exec(`DELETE FROM _conflicts WHERE command_id = ?`, [commandId]);
}

interface Rejection {
  status: number;
  type: string;
  title: string;
  detail: string;
}

function file(store: Store, command: OutboxCommand, rejection: Rejection): void {
  store.exec(
    `INSERT INTO _conflicts
       (command_id, method, path, object, row_id, body, status, type, title, detail, filed_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT (command_id) DO NOTHING`,
    [
      command.commandId,
      command.method,
      command.path,
      command.object,
      command.rowId,
      command.body === null ? null : JSON.stringify(command.body),
      rejection.status,
      rejection.type,
      rejection.title,
      rejection.detail,
      new Date().toISOString(),
    ],
  );
}

function problemField(body: unknown, key: string, fallback: string): string {
  if (!isRecord(body)) return fallback;
  const value = body[key];
  return typeof value === "string" && value !== "" ? value : fallback;
}

function isRecord(value: unknown): value is ServerRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// --- optimistic apply and its undo ---

/** rowSnapshot reads a row exactly as it is stored, for the rollback path.
 *
 * Storage-shaped rather than server-shaped on purpose: these values go back
 * into the same columns they came out of, so they must not be run through
 * `conform` on the way back — a bool is 0/1 down here and `true`/`false` up
 * there, and re-conforming a stored row would reject the replica's own data. */
function rowSnapshot(store: Store, schema: MetaObject, rowId: string): ServerRecord | null {
  const rows = store.query<Record<string, SqlValue>>(
    `SELECT * FROM ${tableName(schema.name)} WHERE id = ?`,
    [rowId],
  );
  return rows.length === 0 ? null : rows[0];
}

function applyOptimistically(
  store: Store,
  index: SchemaIndex,
  schema: MetaObject,
  command: Command,
  before: ServerRecord | null,
): void {
  const table = tableName(schema.name);

  if (command.method === "DELETE") {
    store.exec(`DELETE FROM ${table} WHERE id = ?`, [command.rowId]);
    return;
  }

  const body = command.body ?? {};
  const now = new Date().toISOString();

  if (command.method === "POST") {
    // Deliberately not through applyRecord. That function guards the kernel
    // columns as a *payload* check — a server record without a tenant_id is
    // malformed and must be refused — and an optimistic row is not a payload.
    // Its tenant is whatever this replica has learned (tenantOf), which on a
    // tenant that holds no rows at all is the empty string until the server's
    // own row arrives. Routing this through applyRecord would mean weakening a
    // check that exists for the wire in order to serve a case that never comes
    // off it.
    //
    // `conform` still runs on every field, so INV-T5 is checked on the user's
    // own input before it is stored, not only on its way back.
    const columns = ["id", "tenant_id", "created_at", "updated_at", "archived_at"];
    const values: SqlValue[] = [command.rowId, tenantOf(store, index), now, now, null];
    for (const field of replicaFields(schema)) {
      columns.push(field.name);
      values.push(conform(schema.name, field, body[field.name] ?? null));
    }
    const quoted = columns.map((c) => `"${c}"`).join(", ");
    const placeholders = columns.map(() => "?").join(", ");
    const updates = columns
      .filter((c) => c !== "id")
      .map((c) => `"${c}" = excluded."${c}"`)
      .join(", ");
    store.exec(
      `INSERT INTO ${table} (${quoted}) VALUES (${placeholders})
       ON CONFLICT (id) DO UPDATE SET ${updates}`,
      values,
    );
    return;
  }

  // PATCH: touch only the fields the command carries. A merge-then-applyRecord
  // would have to read the stored row back and re-conform it, which is the
  // storage-shape problem rowSnapshot's comment describes.
  if (before === null) {
    throw new Error(`sync: cannot update ${schema.name} ${command.rowId}: the replica has no such row`);
  }
  const sets: string[] = [`"updated_at" = ?`];
  const values: SqlValue[] = [now];
  for (const field of replicaFields(schema)) {
    if (!(field.name in body)) continue;
    sets.push(`"${field.name}" = ?`);
    values.push(conform(schema.name, field, body[field.name]));
  }
  values.push(command.rowId);
  store.exec(`UPDATE ${table} SET ${sets.join(", ")} WHERE id = ?`, values);
}

/** rollback restores the row this command changed.
 *
 * docs/04 §Upstream 3: "rejected → client rolls back the optimistic rows". For
 * a create that is a delete; for an update or a delete it is the pre-image,
 * because the server state never changed and no amount of pulling would repair
 * the local edit. A rejected update left in place is a replica that disagrees
 * with the server and looks healthy — the one failure mode INV-S3 exists for. */
function rollback(store: Store, schema: MetaObject, command: OutboxCommand): void {
  const table = tableName(schema.name);
  if (command.before === null) {
    store.exec(`DELETE FROM ${table} WHERE id = ?`, [command.rowId]);
    return;
  }

  const columns = Object.keys(command.before);
  const quoted = columns.map((c) => `"${c}"`).join(", ");
  const placeholders = columns.map(() => "?").join(", ");
  const updates = columns
    .filter((c) => c !== "id")
    .map((c) => `"${c}" = excluded."${c}"`)
    .join(", ");
  const values = columns.map((c) => (command.before as Record<string, SqlValue>)[c]);

  store.exec(
    `INSERT INTO ${table} (${quoted}) VALUES (${placeholders})
     ON CONFLICT (id) DO UPDATE SET ${updates}`,
    values,
  );
}

/** tenantOf reads this replica's tenant off any row it already holds.
 *
 * A generated table declares `tenant_id NOT NULL`, so an optimistic create has
 * to supply one, and the worker has no session to read it from — there is no
 * route that returns it and the shell holds it on the other side of the
 * postMessage boundary (WP-2.3-decisions.md §10). Every replicated row carries
 * it, so the replica already knows.
 *
 * The empty answer is very nearly unreachable: a replica exists only after a
 * hydration, so a tenant would have to hold zero rows of every replicable
 * object and then write offline. In that window the row carries `''` and the
 * server's own row overwrites it on acceptance. */
function tenantOf(store: Store, index: SchemaIndex): string {
  for (const schema of index.values()) {
    const rows = store.query<{ tenant_id: string }>(
      `SELECT tenant_id FROM ${tableName(schema.name)} LIMIT 1`,
    );
    if (rows.length > 0 && rows[0].tenant_id) return String(rows[0].tenant_id);
  }
  return "";
}

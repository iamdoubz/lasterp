// SPDX-License-Identifier: AGPL-3.0-only

// The conformance suite for the sync core, written once and run against every
// driver — ADR-017 bar 1 ("one core, two drivers, no conditionals") is only
// demonstrated if the *same assertions* run on both.
//
// It is plain functions rather than vitest cases because one of its two hosts
// is a browser page with no test framework in it. vitest wraps each case in an
// it(); the browser runner calls them in a loop and reports.

import { applyPage, currentCursor, indexSchema, initReplica, rowCount, type FeedPage } from "./core.ts";
import {
  conflicts,
  drain,
  enqueue,
  MAX_ATTEMPTS,
  outboxDepth,
  OutboxFullError,
  OutboxParseError,
  pendingCommands,
  pendingRows,
  UNPERSISTED_OUTBOX_LIMIT,
  type Command,
} from "./outbox.ts";
import { ConformanceError, type MetaObject } from "./schema.ts";
import type { CommandResult, Transport } from "./transport.ts";
import type { Store } from "./port.ts";

/** The schema the suite replicates. Small on purpose, but it carries one of
 * every shape the apply loop treats differently: a plain text column, an enum
 * with a declared option set (INV-T5), and a money field that must stay an
 * exact string. */
export const SUITE_OBJECTS: MetaObject[] = [
  {
    name: "Contact",
    // The bare segment `/meta/objects` publishes (api.ResourcePath), not a full
    // path. The outbox builds its request URLs from this, so a fixture that
    // spelled it differently from the server would let the drain's route
    // construction pass a test it does not deserve.
    resource: "contact",
    module: "contacts",
    persistence: "crud",
    fields: [
      { name: "name", type: "text", required: true },
      { name: "kind", type: "enum", required: true, options: ["customer", "supplier"] },
      { name: "credit_limit", type: "money", required: false },
    ],
  },
  {
    name: "JournalEntry",
    resource: "journalentry",
    module: "ledger",
    persistence: "event_sourced",
    fields: [{ name: "memo", type: "text", required: false }],
  },
];

const TENANT = "t-suite";

/** change builds one feed pointer. */
function change(cursor: number, ref = `r${cursor}`, object = "Contact") {
  return {
    cursor,
    source: "audit",
    ref_id: ref,
    object,
    scope_key: object,
    recorded_at: "2026-08-03T00:00:00Z",
  };
}

/** contact builds one materialised server record. */
function contact(id: string, name = `name-${id}`, extra: Record<string, unknown> = {}) {
  return {
    id,
    tenant_id: TENANT,
    created_at: "2026-08-03T00:00:00Z",
    updated_at: "2026-08-03T00:00:00Z",
    name,
    kind: "customer",
    credit_limit: null,
    ...extra,
  };
}

/** page builds a feed page whose pointers and rows agree, which is what the
 * server sends. Cases that need them to disagree build the page by hand. */
function page(cursors: number[], records = cursors.map((c) => contact(`r${c}`))): FeedPage {
  return {
    data: cursors.map((c) => change(c)),
    cursor: cursors.length > 0 ? cursors[cursors.length - 1] : 0,
    rows: records.length > 0 ? { Contact: records } : {},
  };
}

export interface Case {
  name: string;
  /** Async is allowed for the outbox cases only: the drain is the one part of
   * the core that talks to a transport, so its cases have to await one. Both
   * runners await this, and everything downstream stays synchronous — port.ts
   * is a synchronous interface on purpose. */
  run(store: Store): void | Promise<void>;
}

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

function setUp(store: Store) {
  initReplica(store, SUITE_OBJECTS);
  return indexSchema(SUITE_OBJECTS);
}

// --- outbox fixtures ---

let commandSeq = 0;

/** commandId mints an id shaped like the UUIDv7 the client uses, but ordered
 * rather than random: these cases assert queue order, and a fixture whose ids
 * happened to sort differently from their insertion would make the drain look
 * wrong when the *fixture* was. Real ids come from crypto.randomUUID-class
 * sources; the queue orders by `seq`, never by this. */
function commandId(): string {
  return `018f0000-0000-7000-8000-${String(++commandSeq).padStart(12, "0")}`;
}

function create(rowId: string, name: string): Command {
  return {
    commandId: commandId(),
    method: "POST",
    object: "Contact",
    rowId,
    body: { id: rowId, name, kind: "customer" },
  };
}

function patch(rowId: string, body: Record<string, unknown>): Command {
  return { commandId: commandId(), method: "PATCH", object: "Contact", rowId, body };
}

/** acceptWith builds a Transport whose only working method is `command`: these
 * cases drive the drain, and a downstream call from one would mean the drain
 * reached for something it has no business touching. */
function acceptWith(reply: () => CommandResult): Transport {
  return {
    async meta(): Promise<MetaObject[]> {
      throw new Error("the drain must not fetch metadata");
    },
    async snapshot(): Promise<never> {
      throw new Error("the drain must not hydrate");
    },
    async changes(): Promise<never> {
      throw new Error("the drain must not pull");
    },
    async scope(): Promise<never> {
      throw new Error("the drain must not re-shape");
    },
    async command(): Promise<CommandResult> {
      return reply();
    },
  };
}

function nameOf(store: Store, id: string): string {
  const rows = store.query<{ name: string }>(`SELECT name FROM obj_contact WHERE id = ?`, [id]);
  return rows.length === 0 ? "" : String(rows[0].name);
}

export const cases: Case[] = [
  {
    // INV-S5's client half: what the server ordered is what the replica applies.
    name: "applies a page in order and advances the cursor",
    run(store) {
      const index = setUp(store);
      const cursor = applyPage(store, index, page([1, 2, 3]));
      assert(cursor === 3, `cursor = ${cursor}, want 3`);
      assert(currentCursor(store) === 3, "cursor not persisted");
      assert(rowCount(store, "Contact") === 3, `rows = ${rowCount(store, "Contact")}, want 3`);
    },
  },
  {
    // docs/04: "Crash-safe: cursor advances only after commit."
    name: "a failed page leaves neither rows nor cursor behind",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1]));

      let threw = false;
      try {
        // Descending mid-page. The core must refuse rather than sort it into
        // place or — worse — apply 3 and drop 2 as "already seen", which is how
        // a replica loses an entry the server actually sent.
        applyPage(store, index, page([3, 2]));
      } catch {
        threw = true;
      }
      assert(threw, "a descending page was accepted");
      assert(currentCursor(store) === 1, `cursor = ${currentCursor(store)}, want 1 after rollback`);
      assert(rowCount(store, "Contact") === 1, "rows survived a rolled-back page");
    },
  },
  {
    // At-least-once delivery, exactly-once effect (docs/04 §Guarantees).
    name: "re-applying a page is a no-op",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2]));
      const cursor = applyPage(store, index, page([1, 2]));
      assert(cursor === 2, `cursor = ${cursor}, want 2`);
      assert(rowCount(store, "Contact") === 2, "the page was applied twice");
    },
  },
  {
    // Resume: paging from any position reconstructs the same replica.
    name: "resuming mid-feed converges to the same state as one pass",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2]));
      applyPage(store, index, page([3, 4]));
      applyPage(store, index, page([5, 6]));
      assert(currentCursor(store) === 6, `cursor = ${currentCursor(store)}, want 6`);
      assert(rowCount(store, "Contact") === 6, `rows = ${rowCount(store, "Contact")}, want 6`);
    },
  },
  {
    // A page the client already passed is skipped, not replayed backwards.
    name: "entries at or below the cursor are skipped",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2, 3]));
      const cursor = applyPage(store, index, page([2, 3, 4]));
      assert(cursor === 4, `cursor = ${cursor}, want 4`);
      assert(rowCount(store, "Contact") === 4, `rows = ${rowCount(store, "Contact")}, want 4`);
    },
  },
  {
    name: "an empty page leaves the cursor where it was",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1]));
      const cursor = applyPage(store, index, { data: [], cursor: 1, rows: {} });
      assert(cursor === 1, `cursor = ${cursor}, want 1`);
    },
  },
  {
    // WP-2.4-decisions.md §7. A scoped client's page can be short or empty
    // while the feed has more, because the entries in between were filtered
    // out before it was sent. The cursor cannot be derived from the entries any
    // more, so the server's resume position is taken when it is ahead — without
    // that, the client re-scans the filtered range on every poll for ever.
    name: "a filtered page advances the cursor past what it did not deliver",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1]));
      const cursor = applyPage(store, index, { data: [], cursor: 40, rows: {} });
      assert(cursor === 40, `cursor = ${cursor}, want 40`);
      assert(currentCursor(store) === 40, `stored cursor = ${currentCursor(store)}, want 40`);
    },
  },
  {
    // The other direction is fatal, not clamped. A page reporting a resume
    // point below an entry it just delivered would strand every entry between
    // the two on the next request — the exact loss INV-S5 exists to prevent, so
    // it fails loudly rather than being quietly repaired into something that
    // looks healthy.
    name: "a page resuming behind an entry it delivered is refused",
    run(store) {
      const index = setUp(store);
      let threw = false;
      try {
        applyPage(store, index, { ...page([5, 6]), cursor: 4 });
      } catch {
        threw = true;
      }
      assert(threw, "a page resuming behind its own entries must be refused");
      assert(currentCursor(store) === 0, `cursor = ${currentCursor(store)}, want 0`);
      assert(rowCount(store, "Contact") === 0, "a refused page must leave no rows");
    },
  },
  {
    // Row state is *current* state, so a later value overwrites an earlier one
    // idempotently and last-writer-wins per row (WP-2.2-decisions.md §2).
    name: "a row that changed twice lands on its current value",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1], [contact("r1", "before")]));
      applyPage(store, index, page([2], [contact("r1", "after")]));
      const rows = store.query<{ name: string }>(`SELECT name FROM obj_contact WHERE id = 'r1'`);
      assert(rows.length === 1, `rows = ${rows.length}, want 1`);
      assert(rows[0].name === "after", `name = ${rows[0].name}, want "after"`);
    },
  },
  {
    // WP-2.2-decisions.md §5: a delete the replica is not told about is a
    // divergence no amount of syncing repairs — the one failure mode INV-S3
    // exists to catch. Archived rows arrive flagged and are removed locally.
    name: "an archived row is deleted from the replica",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1, 2], [contact("r1"), contact("r2")]));
      assert(rowCount(store, "Contact") === 2, "setup did not land two rows");

      applyPage(
        store,
        index,
        page([3], [contact("r1", "name-r1", { archived_at: "2026-08-03T01:00:00Z" })]),
      );
      assert(rowCount(store, "Contact") === 1, "archived row was not removed");
      const left = store.query<{ id: string }>(`SELECT id FROM obj_contact`);
      assert(left[0].id === "r2", `surviving row = ${left[0].id}, want r2`);
    },
  },
  {
    // INV-T5 at the replica boundary: a value outside its declared option set
    // is surfaced, not coerced, and the whole page rolls back.
    name: "a non-conforming value rolls the page back instead of being stored",
    run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1], [contact("r1")]));

      let caught: unknown;
      try {
        applyPage(store, index, page([2], [contact("r2", "bad", { kind: "banana" })]));
      } catch (err) {
        caught = err;
      }
      assert(caught instanceof ConformanceError, `want ConformanceError, got ${String(caught)}`);
      assert(currentCursor(store) === 1, `cursor = ${currentCursor(store)}, want 1`);
      assert(rowCount(store, "Contact") === 1, "a rolled-back page left a row behind");
    },
  },
  {
    // Money must stay an exact string. A number here means precision was lost
    // upstream, and storing it would put a wrong amount in the replica.
    name: "a numeric money value is refused",
    run(store) {
      const index = setUp(store);
      let threw = false;
      try {
        applyPage(store, index, page([1], [contact("r1", "n", { credit_limit: 1999 })]));
      } catch (err) {
        threw = err instanceof ConformanceError;
      }
      assert(threw, "a float money value was accepted into the replica");
      assert(rowCount(store, "Contact") === 0, "a refused row was stored anyway");
    },
  },
  {
    // Event-sourced pointers ride in `data` so the cursor stays honest, but
    // they resolve to no row: materialize() only joins audit-sourced entries.
    name: "an event-sourced pointer advances the cursor and stores no row",
    run(store) {
      const index = setUp(store);
      const cursor = applyPage(store, index, {
        data: [{ ...change(1, "e1", "JournalEntry"), source: "event" }],
        cursor: 1,
        rows: {},
      });
      assert(cursor === 1, `cursor = ${cursor}, want 1`);
      assert(rowCount(store, "Contact") === 0, "an event pointer wrote a contact");
    },
  },
  {
    // Rows for an object the replica has no table for would be dropped
    // silently. That is data the client was sent and did not keep — refuse.
    name: "rows for an unknown object are refused, not skipped",
    run(store) {
      const index = setUp(store);
      let threw = false;
      try {
        applyPage(store, index, {
          data: [change(1, "x1", "Mystery")],
          cursor: 1,
          rows: { Mystery: [contact("x1")] },
        });
      } catch {
        threw = true;
      }
      assert(threw, "rows for an unknown object were silently dropped");
    },
  },

  // --- WP-2.3b: the outbox ---

  {
    // docs/04 §Upstream 1: the command is queued *and* the row appears, as one
    // act. A user who wrote something offline and cannot see it has been lied
    // to; a row with no command behind it is work that will never be sent.
    name: "an offline create is queued and visible immediately",
    run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o1", "Offline Ada"), true);

      assert(rowCount(store, "Contact") === 1, "the optimistic row is not in the replica");
      assert(outboxDepth(store) === 1, `outbox depth = ${outboxDepth(store)}, want 1`);
      assert(pendingRows(store, "Contact").join() === "o1", "the row is not flagged pending");

      const queued = pendingCommands(store)[0];
      assert(queued.method === "POST", `method = ${queued.method}`);
      assert(queued.path === "/api/v1/contact", `path = ${queued.path} — the drain would 404`);
      assert(queued.before === null, "a create recorded a pre-image");
    },
  },
  {
    // The accepted path, end to end. The response body is current server state,
    // so applying it *is* the rebase (ADR-004 §Upstream 3) — here the server
    // hands back a name it normalised, and the replica must end up holding the
    // server's version rather than the one the user typed.
    name: "an accepted command clears the outbox and lands the server's version",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o2", "offline ada"), true);

      const report = await drain(
        store,
        index,
        acceptWith(() => ({ status: 201, body: contact("o2", "Offline Ada") })),
      );

      assert(report.accepted === 1, `accepted = ${report.accepted}, want 1`);
      assert(report.conflicted === 0, `conflicted = ${report.conflicted}, want 0`);
      assert(outboxDepth(store) === 0, "an accepted command stayed in the outbox");
      assert(pendingRows(store, "Contact").length === 0, "the pending flag survived acceptance");

      const rows = store.query<{ name: string }>(`SELECT name FROM obj_contact WHERE id = 'o2'`);
      assert(rows[0]?.name === "Offline Ada", `name = ${rows[0]?.name}, want the server's value`);
    },
  },
  {
    // INV-S4, and the rollback docs/04 §Upstream 3 requires. A rejected
    // *update* is the case with teeth: server state never changed, so nothing
    // downstream will ever repair the local edit. Without a pre-image the
    // replica silently disagrees with the server forever.
    name: "a rejected update rolls back to its pre-image and files a conflict",
    async run(store) {
      const index = setUp(store);
      applyPage(store, index, page([1], [contact("o3", "Original")]));

      enqueue(store, index, patch("o3", { name: "Edited offline" }), true);
      assert(nameOf(store, "o3") === "Edited offline", "the optimistic edit is not visible");

      const report = await drain(
        store,
        index,
        acceptWith(() => ({
          status: 422,
          body: {
            type: "about:blank",
            title: "validation failed",
            detail: "field \"name\" is required",
            status: 422,
          },
        })),
      );

      assert(report.conflicted === 1, `conflicted = ${report.conflicted}, want 1`);
      assert(nameOf(store, "o3") === "Original", `name = ${nameOf(store, "o3")}, want the pre-image`);
      assert(outboxDepth(store) === 0, "a rejected command stayed queued");

      const filed = conflicts(store);
      assert(filed.length === 1, `${filed.length} conflicts filed, want 1`);
      assert(filed[0].status === 422, `status = ${filed[0].status}`);
      // The server's own words, not a translation of them.
      assert(filed[0].detail.includes("required"), `detail = ${filed[0].detail}`);
    },
  },
  {
    // A rejected create leaves nothing behind: there was no row before it.
    name: "a rejected create removes its optimistic row",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o4", "Doomed"), true);

      await drain(
        store,
        index,
        acceptWith(() => ({ status: 403, body: { title: "permission denied", status: 403 } })),
      );

      assert(rowCount(store, "Contact") === 0, "a rejected create left its row in the replica");
      assert(conflicts(store).length === 1, "the rejection was not filed");
    },
  },
  {
    // INV-S1. A transport that throws means the request may or may not have
    // arrived, which is the one outcome that must change nothing: the command
    // stays queued and the next drain re-sends it, deduped by command_id
    // (INV-E4). Dropping it here would be the silent loss the invariant names.
    name: "a partitioned drain leaves every command queued",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o5", "Unsent"), true);

      const report = await drain(store, index, acceptWith(() => {
        throw new Error("network down");
      }));

      assert(report.accepted === 0, `accepted = ${report.accepted}, want 0`);
      assert(report.conflicted === 0, "a partition was mistaken for a rejection");
      assert(report.pending === 1, `pending = ${report.pending}, want 1`);
      assert(rowCount(store, "Contact") === 1, "the optimistic row was rolled back by a partition");
    },
  },
  {
    // WP-2.3-decisions.md §11. The gateway raises 409 both for a reused key and
    // for one whose original request is still in flight — the second being the
    // ordinary consequence of the crash the outbox exists to survive. Filing it
    // would show the user a conflict for a command that is succeeding.
    name: "an in-flight idempotency conflict is retried, not filed",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o6", "Racing"), true);

      const report = await drain(
        store,
        index,
        acceptWith(() => ({
          status: 409,
          body: { type: "idempotency-conflict", title: "idempotency key conflict", status: 409 },
        })),
      );

      assert(report.conflicted === 0, "an in-flight key was filed as a rejection");
      assert(report.pending === 1, "the command was not left for the next drain");
      assert(conflicts(store).length === 0, "a spurious conflict reached the tray");
    },
  },
  {
    // …but not forever. A command that will never succeed sits at the head of a
    // strictly-ordered queue blocking everything behind it, which is not a
    // silent drop but is a silent stall — and INV-S4's "surfaced to the user"
    // covers both.
    name: "a command that keeps failing is eventually surfaced",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o7", "Doomed retry"), true);

      const transport = acceptWith(() => ({ status: 503, body: { title: "unavailable" } }));
      for (let i = 0; i < MAX_ATTEMPTS + 1; i++) await drain(store, index, transport);

      assert(outboxDepth(store) === 0, "a permanently failing command never left the queue");
      assert(conflicts(store).length === 1, "it was dropped rather than surfaced");
    },
  },
  {
    // Stop-on-reject (WP-2.3-decisions.md §9). No command runs whose
    // predecessor may have been its precondition, so a rejection halts the
    // drain and everything behind it stays queued rather than being attempted
    // against a state that never happened.
    name: "the drain stops at the first rejection",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o8", "First"), true);
      enqueue(store, index, create("o9", "Second"), true);

      let calls = 0;
      const report = await drain(
        store,
        index,
        acceptWith(() => {
          calls++;
          return { status: 422, body: { title: "validation failed", status: 422 } };
        }),
      );

      assert(calls === 1, `${calls} commands were sent; the drain did not stop at the rejection`);
      assert(report.conflicted === 1, `conflicted = ${report.conflicted}, want 1`);
      assert(report.pending === 1, `pending = ${report.pending}, want 1`);
    },
  },
  {
    // WP-2.3-decisions.md §6: when persistence is *denied* the outbox is capped,
    // because the browser may evict the whole origin without warning and the
    // work in it is the one thing no re-fetch reconstructs.
    name: "an unpersisted outbox is capped and a granted one is not",
    run(store) {
      const index = setUp(store);
      for (let i = 0; i < UNPERSISTED_OUTBOX_LIMIT; i++) {
        enqueue(store, index, create(`cap${i}`, `Capped ${i}`), false);
      }

      let refused = false;
      try {
        enqueue(store, index, create("over", "One too many"), false);
      } catch (err) {
        refused = err instanceof OutboxFullError;
      }
      assert(refused, "the unpersisted outbox accepted a command past its limit");

      // Granted persistence has no cap: a replica that cannot be evicted has no
      // blast radius to bound.
      enqueue(store, index, create("granted", "Persisted"), true);
      assert(
        outboxDepth(store) === UNPERSISTED_OUTBOX_LIMIT + 1,
        `depth = ${outboxDepth(store)}, want the limit plus the persisted write`,
      );
    },
  },
  {
    // An accepted command whose *answer* cannot be applied must still leave the
    // queue. The command succeeded; only the client's ability to reflect it
    // failed (INV-T5 — here, a value outside the option set this replica knows
    // about). Leaving it queued would re-send an applied write on every
    // reconnect forever, because a replay returns the same 2xx and never
    // reaches the retry cap — a stall nothing would ever surface.
    name: "an accepted command whose response will not conform does not stick the queue",
    async run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o11", "Poisonous"), true);

      let threw = false;
      try {
        await drain(
          store,
          index,
          acceptWith(() => ({ status: 201, body: contact("o11", "Poisonous", { kind: "banana" }) })),
        );
      } catch {
        // Loud, which is the point: the same check fails again when the row
        // arrives through the feed.
        threw = true;
      }

      assert(threw, "a non-conforming acceptance was swallowed");
      assert(outboxDepth(store) === 0, "the command is still queued and will be re-sent forever");
    },
  },
  {
    // The port boundary (WP-2.2b-decisions.md §7). Every other read in this
    // client can be cast because the server is the source of truth for it; a
    // corrupt outbox row is work that is gone, so it is refused loudly at the
    // boundary rather than discovered as a malformed request at the wire.
    name: "a corrupt outbox row is refused rather than replayed",
    run(store) {
      const index = setUp(store);
      enqueue(store, index, create("o10", "Readable"), true);

      store.exec(`UPDATE _outbox SET body = 'not json'`);
      let threw = false;
      try {
        pendingCommands(store);
      } catch (err) {
        threw = err instanceof OutboxParseError;
      }
      assert(threw, "an unparseable outbox row was handed back as a command");
    },
  },
];

/** Synthetic feed page for the throughput bench. */
export function benchPage(n: number, from = 0): FeedPage {
  const data = new Array(n);
  const rows = new Array(n);
  for (let i = 0; i < n; i++) {
    const c = from + i + 1;
    data[i] = change(c);
    rows[i] = contact(`r${c}`);
  }
  return { data, cursor: from + n, rows: { Contact: rows } };
}

// SPDX-License-Identifier: AGPL-3.0-only

// The replica, driven from outside for the convergence property test.
//
// internal/app/sync_converge_integrity_test.go spawns this with Node 26 (which
// strips types natively, so there is no build step between the source under
// test and the thing being run), points it at a live server, and compares what
// it dumps to the server's own projection. That test carries INV-S3.
//
// Usage: node converge-driver.ts <base-url> <token> <db-path> [faults...]
//
//   --drop-nth=N            discard every Nth feed entry and its row
//   --fail-after=N          every transport call after the Nth throws (a partition)
//   --kill-in-apply=N       process.exit at the Nth replica row write (a crash)
//   --enqueue=N             queue N offline creates before syncing
//   --enqueue-invalid=N     queue N offline creates the server must reject
//   --label=S               name prefix for queued rows, so the harness can find them
//   --offline               queue the work and stop, without syncing
//   --kill-after-command=N  process.exit after the Nth command's response, before
//                           the outbox row is cleared (the INV-S1 crash window)
//
// The core is imported unmodified. Only the Transport differs from the browser:
// an absolute URL and a bearer token instead of a same-origin cookie, which is
// exactly why Transport is an interface.
//
// WP-2.3a added the partition and crash faults: docs/04 §Test plan asks for "N
// virtual clients × scripted partitions/crashes/interleavings", and a fault has
// to be injected where the client cannot see it coming. WP-2.3b adds the
// upstream half — offline writes to queue, and the one crash window that makes
// INV-S1 falsifiable.

import { writeSync } from "node:fs";

import { openNodeStore } from "./adapters/node.ts";
import { cachedMeta, indexSchema, initReplica } from "./core.ts";
import { newId } from "./ids.ts";
import { conflicts, enqueue, pendingCommands, type Command } from "./outbox.ts";
import { sync } from "./replica.ts";
import { replicableObjects, tableName, type MetaObject } from "./schema.ts";
import { DeviceWipedError } from "./wipe.ts";
import type { FeedPage } from "./core.ts";
import type { Store } from "./port.ts";
import type { CommandResult, ReplayableCommand, SnapshotPage, Transport } from "./transport.ts";

const [baseURL, token, dbPath, ...flags] = process.argv.slice(2);

/** flag reads `--name=N`, defaulting to 0 (off). */
function flag(name: string): number {
  const prefix = `--${name}=`;
  return Number(flags.find((f) => f.startsWith(prefix))?.slice(prefix.length) ?? 0);
}

/** textFlag reads `--name=S`. */
function textFlag(name: string, fallback: string): string {
  const prefix = `--${name}=`;
  return flags.find((f) => f.startsWith(prefix))?.slice(prefix.length) ?? fallback;
}

/** dropNth simulates a feed that skips entries: every Nth change (and its rows)
 * is discarded before the core ever sees it.
 *
 * This is WP-2.2b-decisions.md §8.1. A convergence test whose oracle is current
 * server state, driven by an apply loop that writes current server state, can
 * pass by construction — the final pass re-fetches over any earlier damage. If
 * the property still holds with entries deleted, it is not testing selection,
 * and the Go side asserts that this run **fails**. */
const dropNth = flag("drop-nth");

/** failAfter simulates a partition: the first N transport calls succeed and
 * every one after that throws.
 *
 * The error path is the point. A client that loses the network mid-sync must
 * leave the replica at a consistent position rather than a half-applied one, and
 * the only way to know that is to cut the wire somewhere the core did not
 * choose. The driver still dumps afterwards — a partitioned client is *partway*,
 * not broken, and the harness asserts it converges once the wire is back. */
const failAfter = flag("fail-after");

/** killInApply simulates a crash: process.exit at the Nth replica row write,
 * which is inside the apply transaction and before the cursor moves.
 *
 * This is the one fault that cannot be injected through the Transport, because
 * the window it targets is inside `store.transaction`. core.ts claims the
 * cursor and its rows move together or not at all; this is what makes that
 * claim falsifiable instead of a comment.
 *
 * N counts row writes rather than transactions because a caller that wants to
 * land inside the window needs to name a point within it, and "the 2nd row of
 * this page" is that. The caller is responsible for there being N rows to
 * write — the harness guarantees a floor, see crashFloor. */
const killInApply = flag("kill-in-apply");

/** enqueueCount / enqueueInvalid / label / offline are the upstream half: what
 * the user did while disconnected.
 *
 * `enqueueInvalid` queues a command the **server** must refuse while the client
 * cannot tell — a syntactically fine email that is not an address. That gap is
 * the design, not a trick: the client does not re-validate business rules,
 * because the server is the referee (ADR-004), so the only honest way to
 * produce a rejection is to send something only the server knows is wrong.
 * Anything the replica could have caught (an out-of-set enum, say) is refused
 * by `conform` at enqueue time and never becomes a command at all. */
const enqueueCount = flag("enqueue");
const enqueueInvalid = flag("enqueue-invalid");
const label = textFlag("label", "Offline");
const offline = flags.includes("--offline");

/** killAfterCommand exits after the Nth command's response has been read and
 * *before* the transaction that clears its outbox row commits.
 *
 * This is the crash window INV-S1 is about, and nothing else reaches it: the
 * server has committed, the client has the answer in hand, and the record of
 * having sent it is not yet durable. A client that lost this race must re-send
 * on the next drain and be deduplicated by the gateway (INV-E4) — never lose
 * the write, never apply it twice. Either failure is invisible without killing
 * the process exactly here. */
const killAfterCommand = flag("kill-after-command");

/** CRASH_MARKER is what a simulated crash prints to stderr immediately before
 * dying. The Go harness (sync_converge_integrity_test.go) matches on it. */
const CRASH_MARKER = "lasterp-sync-crash-injected";

/** PARTITION_MARKER is written to stderr when a simulated partition actually
 * fires. Like CRASH_MARKER it exists so the harness can assert the fault
 * happened: a fault that silently does not fire turns a fault-injection suite
 * into an ordinary one that still reports green. */
const PARTITION_MARKER = "lasterp-sync-partition-injected";

/** WIPE_MARKER is written when a remote wipe was received and honored (WP-2.5).
 * Like the markers above it exists so the harness can assert the event actually
 * happened: an empty replica proves a wipe only if a wipe is what emptied it. */
const WIPE_MARKER = "lasterp-device-wiped";

/** Partitioned marks the simulated network failure, so the driver can tell it
 * apart from a real defect and still dump. Anything else propagates. */
class Partitioned extends Error {}

let calls = 0;

async function fetchJSON<T>(path: string): Promise<T> {
  const response = await fetch(baseURL + path, {
    headers: { Accept: "application/json", Authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    const text = await response.text();
    // This driver has its own fetch rather than api/client.ts (absolute URL and
    // a bearer token instead of a same-origin cookie), so it must repeat the
    // one translation that matters: a wipe 401 is not a request failure, it is
    // an instruction (WP-2.5). Without this the driver would report a generic
    // error and never exercise the client-side wipe at all.
    if (response.status === 401 && text.includes(`"device-wiped"`)) {
      throw new DeviceWipedError();
    }
    throw new Error(`${path} -> ${response.status} ${text}`);
  }
  return (await response.json()) as T;
}

async function get<T>(path: string): Promise<T> {
  if (failAfter > 0 && ++calls > failAfter) {
    throw new Partitioned(`simulated partition after ${failAfter} calls`);
  }
  return fetchJSON<T>(path);
}

let seen = 0;

const transport: Transport = {
  async meta(): Promise<MetaObject[]> {
    return (await get<{ data: MetaObject[] }>("/api/v1/meta/objects")).data;
  },

  async snapshot(object: string, after: string, limit: number): Promise<SnapshotPage> {
    const params = new URLSearchParams({ object, limit: String(limit) });
    if (after !== "") params.set("after", after);
    return get<SnapshotPage>(`/api/v1/sync/snapshot?${params}`);
  },

  async changes(after: number, limit: number): Promise<FeedPage> {
    const params = new URLSearchParams({
      after: String(after),
      limit: String(limit),
      include: "rows",
    });
    const page = await get<FeedPage>(`/api/v1/sync/changes?${params}`);
    return dropNth > 0 ? withDroppedEntries(page) : page;
  },

  async scope(): Promise<string[]> {
    return (await get<{ data: string[] }>("/api/v1/sync/scope")).data;
  },

  // The drain, over the wire, into the ordinary route. Note what is absent:
  // there is no sync command endpoint to call, because WP-2.3-decisions.md §1
  // did not build one — this issues the same POST /api/v1/contact the online UI
  // issues, and INV-S2 holds because there is nothing else to issue.
  async command(command: ReplayableCommand): Promise<CommandResult> {
    if (failAfter > 0 && ++calls > failAfter) {
      throw new Partitioned(`simulated partition after ${failAfter} calls`);
    }

    const response = await fetch(baseURL + command.path, {
      method: command.method,
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        // The command_id *is* the Idempotency-Key (§1). One identifier, so the
        // gateway's reserve-first store is what makes a re-send after a crash
        // exactly-once (INV-E4) rather than something this client implements.
        "Idempotency-Key": command.commandId,
      },
      body: command.body === null ? undefined : JSON.stringify(command.body),
    });

    const text = await response.text();
    // Same translation as the GET path above, and for the same reason the real
    // transport does it: a wipe must throw past the drain rather than be filed
    // as this command's rejection (WP-2.5-decisions.md §6).
    if (response.status === 401 && text.includes(`"device-wiped"`)) {
      throw new DeviceWipedError();
    }
    const result: CommandResult = {
      status: response.status,
      body: text === "" ? null : (JSON.parse(text) as unknown),
    };

    if (killAfterCommand > 0 && ++commandsSent >= killAfterCommand) {
      // The server has committed and this process is about to die holding the
      // only record that it did. See killAfterCommand.
      writeSync(2, `${CRASH_MARKER}\n`);
      process.exit(9);
    }
    return result;
  },
};

let commandsSent = 0;

/** withDroppedEntries removes every Nth entry *and the row it names*, which is
 * what a feed that skipped a commit would look like to a client. Dropping the
 * pointer alone would leave the row in `rows` and the replica would still
 * converge — the mutation has to remove both to model the real failure. */
function withDroppedEntries(page: FeedPage): FeedPage {
  const kept: FeedPage["data"] = [];
  const dropped = new Set<string>();
  for (const change of page.data) {
    seen++;
    if (seen % dropNth === 0) {
      dropped.add(change.ref_id);
      continue;
    }
    kept.push(change);
  }

  const rows: NonNullable<FeedPage["rows"]> = {};
  for (const [object, records] of Object.entries(page.rows ?? {})) {
    rows[object] = records.filter((r) => !dropped.has(String(r["id"])));
  }
  return { data: kept, cursor: page.cursor, rows };
}

/** killing wraps a Store so the process dies inside the Nth apply transaction.
 *
 * It counts row writes rather than transactions because that is where the
 * window actually is: `applyPage` writes rows and then the cursor, both inside
 * one transaction, so exiting between the two is the state the crash-safety
 * claim is about. SQLite rolls the open transaction back when the file is next
 * opened, and the harness reopens and asserts the replica still converges. */
function killing(store: Store, after: number): Store {
  if (after <= 0) return store;
  let writes = 0;
  return {
    ...store,
    exec(sql, params) {
      if (sql.startsWith("INSERT INTO obj_") && ++writes >= after) {
        // Not an exception: an exception unwinds and SQLite commits nothing but
        // the process lives, which is a rollback, not a crash. Only a real exit
        // leaves the file the way a killed browser tab does.
        //
        // The marker goes out through fs.writeSync rather than
        // process.stderr.write because the latter is asynchronous on a Windows
        // pipe and would be lost to the exit. The harness keys on this string,
        // not on the exit code: killing the process from inside a synchronous
        // SQLite call trips a libuv assertion on Windows, so the status is
        // 0xc0000409 there and 9 on Linux. The marker is the same everywhere,
        // and — unlike a bare non-zero exit — it cannot be produced by a driver
        // that failed for a real reason.
        writeSync(2, `${CRASH_MARKER}\n`);
        process.exit(9);
      }
      store.exec(sql, params);
    },
  };
}

// The object list is fetched before any fault can fire, so a partitioned run
// still knows what to dump. It costs one request and removes the alternative,
// which was reconstructing the object list from sqlite_master and mapping table
// names back — thirty lines to work around a wire this driver cut itself.
const store = openNodeStore(dbPath);

// The object list, fetched before any fault can fire so a partitioned run still
// knows what to dump.
//
// A **wiped** device cannot fetch it at all — the server refuses every request,
// which is the whole of INV-D1 — so it falls back to the schema this replica
// cached, exactly as the real client does in replica.ts `open()`. Without the
// fallback the driver dies here with an unhandled error and the wipe tests pass
// or fail for reasons unrelated to the wipe. The list is read *before* sync()
// runs, so it still names the tables afterwards even though the wipe clears
// `_meta` along with everything else.
let metaObjects: MetaObject[];
try {
  metaObjects = (await fetchJSON<{ data: MetaObject[] }>("/api/v1/meta/objects")).data;
} catch (err) {
  if (!(err instanceof DeviceWipedError)) throw err;
  metaObjects = cachedMeta(store) ?? [];
}
const objects = replicableObjects(metaObjects);
try {
  if (enqueueCount > 0 || enqueueInvalid > 0) {
    // initReplica before sync() would run it: an offline client queues work
    // into a replica it already has, and --offline never reaches open().
    initReplica(store, metaObjects);
    const index = indexSchema(metaObjects);
    for (const command of offlineCommands()) {
      // persisted = true: the cap is a browser-storage policy (§6) and this
      // driver has no OPFS to be evicted from. Its own test asserts the cap.
      enqueue(store, index, command, true);
    }
  }

  if (!offline) {
    try {
      // The crash wrapper goes on *here*, not around the enqueue above.
      // killInApply counts `INSERT INTO obj_` statements, and an optimistic
      // create is one — so wrapping the store from the start meant a client
      // scheduled to crash mid-apply died while queueing instead, rolling back
      // work its user had already done and taking the fault nowhere near the
      // window it was aimed at. Queueing is not part of the sync under test.
      await sync(killing(store, killInApply), transport);
    } catch (err) {
      // A remote wipe is a *successful* outcome for this driver, not a failure:
      // sync() has already destroyed the replica by the time it rethrows
      // (WP-2.5). The marker lets the Go harness assert the wipe actually fired
      // rather than inferring it from an empty dump — an empty replica is also
      // what a driver that never synced produces, and a wipe test that passes
      // against a replica which was never populated is testing nothing.
      if (err instanceof DeviceWipedError) {
        writeSync(2, `${WIPE_MARKER}\n`);
      } else if (err instanceof Partitioned) {
        // A partition leaves a partway replica, which is a legitimate state to
        // compare against: the harness reconnects and requires convergence then.
        writeSync(2, `${PARTITION_MARKER}\n`);
      } else {
        throw err;
      }
    }
  }

  // Dump every replica table, ordered by id, for the Go side to compare, plus
  // the outbox bookkeeping so the upstream properties are assertable: INV-S4's
  // conservation is a statement about these three tables' counts.
  const dump: Record<string, unknown[]> = {};
  for (const object of objects) {
    dump[object.name] = store.query(`SELECT * FROM ${tableName(object.name)} ORDER BY id`);
  }
  dump["_outbox"] = pendingCommands(store);
  dump["_conflicts"] = conflicts(store);
  dump["_pending"] = store.query(`SELECT * FROM _pending ORDER BY object, row_id`);
  // WP-2.4: `_hydration` is what this replica claims to hold, so it is how the
  // scope tests read the re-shape — an object purged by a revocation leaves
  // here as well as leaving its table (WP-2.4-decisions.md §3).
  dump["_hydration"] = store.query(`SELECT * FROM _hydration ORDER BY object`);
  process.stdout.write(JSON.stringify(dump));
} finally {
  store.close();
}

/** offlineCommands is the work the user did while disconnected: valid creates,
 * then any the server is required to refuse.
 *
 * Ids are minted here because the client owns them (§2) — the row the user sees
 * offline is the row the server ends up with, so there is nothing to rewrite on
 * acceptance and nothing for a queued command to point at that later moves. */
function* offlineCommands(): Generator<Command> {
  for (let i = 0; i < enqueueCount; i++) {
    const id = newId();
    yield {
      commandId: newId(),
      method: "POST",
      object: "Contact",
      rowId: id,
      body: { id, name: `${label} ${i}`, email: `${id}@example.test`, kind: "customer" },
    };
  }
  for (let i = 0; i < enqueueInvalid; i++) {
    const id = newId();
    yield {
      commandId: newId(),
      method: "POST",
      object: "Contact",
      rowId: id,
      // A string, so `conform` accepts it; not an address, so the server does
      // not. See enqueueInvalid.
      body: { id, name: `${label} invalid ${i}`, email: "not-an-address", kind: "customer" },
    };
  }
}

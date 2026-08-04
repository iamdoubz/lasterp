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
//   --drop-nth=N       discard every Nth feed entry and its row
//   --fail-after=N     every transport call after the Nth throws (a partition)
//   --kill-in-apply=N  process.exit at the Nth replica row write (a crash)
//
// The core is imported unmodified. Only the Transport differs from the browser:
// an absolute URL and a bearer token instead of a same-origin cookie, which is
// exactly why Transport is an interface.
//
// WP-2.3a adds the last two: docs/04 §Test plan asks for "N virtual clients ×
// scripted partitions/crashes/interleavings", and a fault has to be injected
// where the client cannot see it coming. Both are flags on this driver rather
// than a second harness, because the thing under test is the same core reached
// the same way — only the weather differs.

import { writeSync } from "node:fs";

import { openNodeStore } from "./adapters/node.ts";
import { sync } from "./replica.ts";
import { replicableObjects, tableName, type MetaObject } from "./schema.ts";
import type { FeedPage } from "./core.ts";
import type { Store } from "./port.ts";
import type { SnapshotPage, Transport } from "./transport.ts";

const [baseURL, token, dbPath, ...flags] = process.argv.slice(2);

/** flag reads `--name=N`, defaulting to 0 (off). */
function flag(name: string): number {
  const prefix = `--${name}=`;
  return Number(flags.find((f) => f.startsWith(prefix))?.slice(prefix.length) ?? 0);
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

/** CRASH_MARKER is what a simulated crash prints to stderr immediately before
 * dying. The Go harness (sync_converge_integrity_test.go) matches on it. */
const CRASH_MARKER = "lasterp-sync-crash-injected";

/** PARTITION_MARKER is written to stderr when a simulated partition actually
 * fires. Like CRASH_MARKER it exists so the harness can assert the fault
 * happened: a fault that silently does not fire turns a fault-injection suite
 * into an ordinary one that still reports green. */
const PARTITION_MARKER = "lasterp-sync-partition-injected";

/** Partitioned marks the simulated network failure, so the driver can tell it
 * apart from a real defect and still dump. Anything else propagates. */
class Partitioned extends Error {}

let calls = 0;

async function fetchJSON<T>(path: string): Promise<T> {
  const response = await fetch(baseURL + path, {
    headers: { Accept: "application/json", Authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    throw new Error(`${path} -> ${response.status} ${await response.text()}`);
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
};

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
const objects = replicableObjects((await fetchJSON<{ data: MetaObject[] }>("/api/v1/meta/objects")).data);

const store = killing(openNodeStore(dbPath), killInApply);
try {
  try {
    await sync(store, transport);
  } catch (err) {
    // A partition leaves a partway replica, which is a legitimate state to
    // compare against: the harness reconnects and requires convergence then.
    if (!(err instanceof Partitioned)) throw err;
    writeSync(2, `${PARTITION_MARKER}\n`);
  }

  // Dump every replica table, ordered by id, for the Go side to compare.
  const dump: Record<string, unknown[]> = {};
  for (const object of objects) {
    dump[object.name] = store.query(`SELECT * FROM ${tableName(object.name)} ORDER BY id`);
  }
  process.stdout.write(JSON.stringify(dump));
} finally {
  store.close();
}

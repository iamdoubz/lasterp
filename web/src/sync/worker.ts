// SPDX-License-Identifier: AGPL-3.0-only

// The replica worker: the only place SQLite-WASM is instantiated.
//
// It lives in a dedicated worker because it has to — both OPFS VFSes rest on
// FileSystemFileHandle.createSyncAccessHandle(), which browsers do not expose
// on the main thread. ADR-017 §Consequences records that this is a platform
// constraint and would have bound a Rust core identically.
//
// Everything inside is synchronous (port.ts is a synchronous interface on
// purpose — an async port would make the transaction boundary something you can
// accidentally await across). The async surface is exactly this file's
// postMessage boundary and no deeper.

import { openOpfsStore, requestPersistence, ReplicaLockedError, wipePool } from "./adapters/opfs.ts";
import { DeviceWipedError } from "./wipe.ts";
import { currentCursor, type SchemaIndex } from "./core.ts";
import {
  conflicts,
  discardConflict,
  enqueue,
  outboxDepth,
  UNPERSISTED_OUTBOX_LIMIT,
} from "./outbox.ts";
import { hydrated, open, sync } from "./replica.ts";
import { httpTransport } from "./transport.ts";
import type { SyncRequest, SyncResponse, SyncStatus } from "./protocol.ts";
import { tableName } from "./schema.ts";
import type { Store } from "./port.ts";

interface Replica {
  store: Store;
  index: SchemaIndex;
}

let replicaState: Replica | undefined;
let persisted = false;
const transport = httpTransport();

/** replica opens the database and generates its schema, once per worker.
 *
 * The schema is resolved here rather than only inside `sync` because a write
 * needs it and a write must work offline: `open` falls back to the cached
 * metadata when the server is unreachable, so a tab reloaded on a train can
 * still queue a command. */
async function replica(): Promise<Replica> {
  if (replicaState === undefined) {
    // Persistence is requested before the replica exists, so a denial is known
    // by the time anything is written and the shell can say so up front — which
    // is the only honest moment for that warning, since eviction clears the
    // whole origin and there is no afterwards in which to apologise.
    persisted = await requestPersistence();
    const store = await openOpfsStore();
    replicaState = { store, index: await open(store, transport) };
  }
  return replicaState;
}

/** wipeStorage finishes what wipeReplica starts (WP-2.5-decisions.md §7).
 *
 * `wipeReplica` has already deleted every row by the time this runs, and that
 * is **not enough on its own**: SQLite frees pages into the file's freelist
 * rather than to the filesystem, so the data is still sitting in the OPFS file
 * afterwards — readable by whoever has the machine, which on a stolen device is
 * the whole threat. `wipePool` is what actually returns the storage.
 *
 * The store is closed and forgotten first: the pool holds its access handles
 * exclusively, and wiping underneath an open connection is how a half-erased
 * file gets left behind. Dropping `replicaState` also means the next request
 * re-opens — into a server that keeps refusing, so the device is told again.
 * That is the idempotence §7 relies on instead of atomicity it cannot have. */
async function wipeStorage(): Promise<void> {
  const state = replicaState;
  replicaState = undefined;
  try {
    state?.store.close();
  } catch {
    // Already closed, or closing failed — either way the pool wipe below is
    // the operation that matters and it must still be attempted.
  }
  await wipePool();
}

async function handle(request: SyncRequest): Promise<unknown> {
  switch (request.kind) {
    case "sync":
      try {
        return await sync((await replica()).store, transport);
      } catch (err) {
        // sync() has already emptied the tables by the time it rethrows; this
        // is the half that needs the adapter, which the core deliberately
        // cannot reach through the port (INV-D1, WP-2.5).
        if (err instanceof DeviceWipedError) await wipeStorage();
        throw err;
      }

    case "status": {
      const { store } = await replica();
      const status: SyncStatus = {
        hydrated: hydrated(store),
        cursor: currentCursor(store),
        persisted,
        pending: outboxDepth(store),
        conflicts: conflicts(store).length,
        limit: persisted ? null : UNPERSISTED_OUTBOX_LIMIT,
      };
      return status;
    }

    case "list": {
      const { store } = await replica();
      // The table name comes from the object name the same way the generator
      // built it, so a request for an object with no table is a missing-table
      // error rather than an injection surface.
      return store.query(`SELECT * FROM ${tableName(request.object)} ORDER BY id`);
    }

    case "write": {
      const { store, index } = await replica();
      enqueue(store, index, request.command, persisted);
      return undefined;
    }

    case "conflicts": {
      const { store } = await replica();
      return conflicts(store);
    }

    case "discard": {
      const { store } = await replica();
      discardConflict(store, request.commandId);
      return undefined;
    }
  }
}

self.onmessage = (event: MessageEvent<SyncRequest>) => {
  const request = event.data;
  handle(request).then(
    (value) => reply({ id: request.id, ok: true, value }),
    (err: unknown) => {
      const name =
        err instanceof ReplicaLockedError ? "ReplicaLockedError"
        : err instanceof Error ? err.name
        : "Error";
      reply({
        id: request.id,
        ok: false,
        name,
        message: err instanceof Error ? err.message : String(err),
      });
    },
  );
};

function reply(response: SyncResponse): void {
  self.postMessage(response);
}

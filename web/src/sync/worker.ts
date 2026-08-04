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

import { openOpfsStore, requestPersistence, ReplicaLockedError } from "./adapters/opfs.ts";
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

async function handle(request: SyncRequest): Promise<unknown> {
  switch (request.kind) {
    case "sync":
      return sync((await replica()).store, transport);

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

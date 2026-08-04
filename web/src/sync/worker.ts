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
import { currentCursor } from "./core.ts";
import { hydrated, sync } from "./replica.ts";
import { httpTransport } from "./transport.ts";
import type { SyncRequest, SyncResponse, SyncStatus } from "./protocol.ts";
import { tableName } from "./schema.ts";
import type { Store } from "./port.ts";

let store: Store | undefined;
let persisted = false;
const transport = httpTransport();

async function replica(): Promise<Store> {
  if (store === undefined) {
    // Persistence is requested before the replica exists, so a denial is known
    // by the time anything is written and the shell can say so up front.
    persisted = await requestPersistence();
    store = await openOpfsStore();
  }
  return store;
}

async function handle(request: SyncRequest): Promise<unknown> {
  switch (request.kind) {
    case "sync":
      return sync(await replica(), transport);

    case "status": {
      const db = await replica();
      const status: SyncStatus = {
        hydrated: hydrated(db),
        cursor: currentCursor(db),
        persisted,
      };
      return status;
    }

    case "list": {
      const db = await replica();
      // The table name comes from the object name the same way the generator
      // built it, so a request for an object with no table is a missing-table
      // error rather than an injection surface.
      return db.query(`SELECT * FROM ${tableName(request.object)} ORDER BY id`);
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

// SPDX-License-Identifier: AGPL-3.0-only

// The main-thread half of the worker boundary: a typed async client over
// postMessage, so screens never learn that a worker exists.
//
// This is the seam WP-1.5 left for Phase 2 — web/src/api/client.ts is "the one
// place the client talks to the server", and a replica read replaces that
// transport without a screen changing (WP-1.5-decisions.md §1).

import type { SyncCommand, SyncResponse, SyncStatus } from "./protocol.ts";

/** ReplicaLocked is the host-side counterpart of the worker's
 * ReplicaLockedError: another tab holds the replica. It is a distinct type
 * because the shell renders it as a state ("open in another tab") rather than
 * as a failure — see WP-2.2b-decisions.md §5. */
export class ReplicaLocked extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ReplicaLocked";
  }
}

/** SyncClient is everything the UI can ask of the replica. Deliberately small:
 * this WP's replica is read-only, and the outbox that would widen it is
 * WP-2.3's. */
export interface SyncClient {
  /** Hydrate if needed, then catch up. Returns the cursor reached. */
  sync(): Promise<number>;
  status(): Promise<SyncStatus>;
  list<T = Record<string, unknown>>(object: string): Promise<T[]>;
  close(): void;
}

/** WorkerLike is the part of Worker this file uses. It exists so the
 * correlation logic below can be tested without a browser — the logic is not
 * about workers, it is about matching replies to requests and failing the
 * outstanding ones on close, and neither needs a real thread. */
export interface WorkerLike {
  postMessage(message: unknown): void;
  terminate(): void;
  onmessage: ((event: MessageEvent<SyncResponse>) => void) | null;
}

/** spawnWorker builds the real replica worker.
 *
 * The URL is built with `new URL(..., import.meta.url)` so the bundler emits it
 * as a real worker chunk; a bare string would break under Vite's asset hashing
 * in a production build. */
function spawnWorker(): WorkerLike {
  return new Worker(new URL("./worker.ts", import.meta.url), { type: "module" });
}

/** startReplica boots the worker and returns a client for it. */
export function startReplica(spawn: () => WorkerLike = spawnWorker): SyncClient {
  const worker = spawn();
  const pending = new Map<number, { resolve(v: unknown): void; reject(e: unknown): void }>();
  let nextId = 1;

  worker.onmessage = (event: MessageEvent<SyncResponse>) => {
    const response = event.data;
    const waiter = pending.get(response.id);
    if (waiter === undefined) return;
    pending.delete(response.id);

    if (response.ok) {
      waiter.resolve(response.value);
      return;
    }
    waiter.reject(
      response.name === "ReplicaLockedError"
        ? new ReplicaLocked(response.message)
        : new Error(response.message),
    );
  };

  function send<T>(command: SyncCommand): Promise<T> {
    const id = nextId++;
    return new Promise<T>((resolve, reject) => {
      pending.set(id, { resolve: resolve as (v: unknown) => void, reject });
      worker.postMessage({ ...command, id });
    });
  }

  return {
    sync: () => send<number>({ kind: "sync" }),
    status: () => send<SyncStatus>({ kind: "status" }),
    list: <T>(object: string) => send<T[]>({ kind: "list", object }),
    close: () => {
      // Reject anything outstanding rather than leaving promises that never
      // settle — a screen awaiting a reply from a terminated worker would hang.
      for (const waiter of pending.values()) waiter.reject(new Error("sync: replica closed"));
      pending.clear();
      worker.terminate();
    },
  };
}

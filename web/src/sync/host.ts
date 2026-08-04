// SPDX-License-Identifier: AGPL-3.0-only

// The main-thread half of the worker boundary: a typed async client over
// postMessage, so screens never learn that a worker exists.
//
// This is the seam WP-1.5 left for Phase 2 — web/src/api/client.ts is "the one
// place the client talks to the server", and a replica read replaces that
// transport without a screen changing (WP-1.5-decisions.md §1).

import type { Command, Conflict } from "./outbox.ts";
import type { SyncCommand, SyncResponse, SyncStatus } from "./protocol.ts";

/** rehydrate turns the worker's serialised error back into the type the shell
 * branches on. Structured clone drops the prototype, so the name is carried
 * explicitly and mapped here — an `instanceof` check on the far side of
 * postMessage silently fails, and a locked replica or a full outbox would
 * arrive as a generic failure with a state nobody rendered. */
function rehydrate(name: string, message: string): Error {
  switch (name) {
    case "ReplicaLockedError":
      return new ReplicaLocked(message);
    case "OutboxFullError":
      return new OutboxFull(message);
    default:
      return new Error(message);
  }
}

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

/** OutboxFull is the host-side counterpart of the worker's OutboxFullError: the
 * browser refused persistent storage and there are now as many unsent commands
 * as WP-2.3-decisions.md §6 is willing to risk. Distinct for the same reason as
 * ReplicaLocked — the shell renders it as a state ("go online to send your
 * work") rather than as a save that failed for unknown reasons. */
export class OutboxFull extends Error {
  constructor(message: string) {
    super(message);
    this.name = "OutboxFull";
  }
}

/** SyncClient is everything the UI can ask of the replica. */
export interface SyncClient {
  /** Drain the outbox, hydrate if needed, then catch up. Returns the cursor
   * reached. This is the reconnect path, in the order WP-2.3-decisions.md §4
   * settles. */
  sync(): Promise<number>;
  status(): Promise<SyncStatus>;
  list<T = Record<string, unknown>>(object: string): Promise<T[]>;
  /** Queue a write and apply it optimistically. Rejects with OutboxFull when an
   * unpersisted replica is at its cap. */
  write(command: Command): Promise<void>;
  /** The tray: rejections the server explained and the user has not dealt
   * with. */
  conflicts(): Promise<Conflict[]>;
  /** Drop one filed rejection. The only way a command leaves the system without
   * reaching the server — loudly, and by the person whose work it is. */
  discard(commandId: string): Promise<void>;
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
    waiter.reject(rehydrate(response.name, response.message));
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
    write: (command: Command) => send<void>({ kind: "write", command }),
    conflicts: () => send<Conflict[]>({ kind: "conflicts" }),
    discard: (commandId: string) => send<void>({ kind: "discard", commandId }),
    close: () => {
      // Reject anything outstanding rather than leaving promises that never
      // settle — a screen awaiting a reply from a terminated worker would hang.
      for (const waiter of pending.values()) waiter.reject(new Error("sync: replica closed"));
      pending.clear();
      worker.terminate();
    },
  };
}

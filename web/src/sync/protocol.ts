// SPDX-License-Identifier: AGPL-3.0-only

// The UI ⇄ worker message protocol.
//
// It exists as its own module so neither side imports the other: the worker
// must not pull in the code that constructs a Worker, and the host must not
// pull in SQLite-WASM. ADR-017 §Consequences requires this boundary — both OPFS
// VFSes rest on createSyncAccessHandle(), which browsers expose only in a
// dedicated worker — and requires the surface across it to be async even though
// the core inside it is synchronous.

import type { Command } from "./outbox.ts";

/** What the host can ask of the replica worker.
 *
 * `write` is the whole upstream surface: one queued command, applied
 * optimistically and sent on the next drain. There is deliberately no "send
 * this now" variant — an offline-first client has one write path and the
 * network is an optimization (commandment 4), so a screen that could choose
 * would be a screen that behaves differently offline. */
export type SyncCommand =
  | { kind: "sync" }
  | { kind: "status" }
  | { kind: "list"; object: string }
  | { kind: "write"; command: Command }
  | { kind: "conflicts" }
  | { kind: "discard"; commandId: string };

/** A command with its correlation id. This is an intersection rather than a
 * union carrying `id` in each arm because `Omit<Union, "id">` collapses a union
 * to its common keys — the host would lose `object` off the list command. */
export type SyncRequest = SyncCommand & { id: number };

/** What the replica knows about itself. */
export interface SyncStatus {
  /** Every replicable object has finished its snapshot. */
  hydrated: boolean;
  /** The feed position the replica has applied up to. */
  cursor: number;
  /** navigator.storage.persist() was granted. When false the browser may evict
   * this replica without warning — and since WP-2.3b that replica holds work
   * the server has never seen, which is the one thing no re-fetch reconstructs
   * (WP-2.2b-decisions.md §7). */
  persisted: boolean;
  /** Commands queued and not yet accepted. Shown from the first write rather
   * than at the limit: the warning has to be true before anything is at risk,
   * which is the only moment it can be (WP-2.3-decisions.md §6). */
  pending: number;
  /** Rejections waiting in the tray. */
  conflicts: number;
  /** The cap on `pending`, or null when persistence was granted and there is
   * none — a replica that cannot be evicted has no blast radius to bound. */
  limit: number | null;
}

/** Responses the worker sends back.
 *
 * Errors cross as a serialised shape rather than an Error instance: structured
 * clone drops the prototype, so an `instanceof` check on the far side would
 * silently fail and a locked replica would look like a generic failure. `name`
 * is carried explicitly so the host can tell ReplicaLockedError from the rest. */
export type SyncResponse =
  | { id: number; ok: true; value: unknown }
  | { id: number; ok: false; name: string; message: string };

// SPDX-License-Identifier: AGPL-3.0-only

// The UI ⇄ worker message protocol.
//
// It exists as its own module so neither side imports the other: the worker
// must not pull in the code that constructs a Worker, and the host must not
// pull in SQLite-WASM. ADR-017 §Consequences requires this boundary — both OPFS
// VFSes rest on createSyncAccessHandle(), which browsers expose only in a
// dedicated worker — and requires the surface across it to be async even though
// the core inside it is synchronous.

/** What the host can ask of the replica worker. */
export type SyncCommand =
  | { kind: "sync" }
  | { kind: "status" }
  | { kind: "list"; object: string };

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
   * this replica without warning; for a read-only replica that costs a
   * re-hydration (WP-2.2b-decisions.md §7). */
  persisted: boolean;
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

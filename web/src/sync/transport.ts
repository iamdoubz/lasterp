// SPDX-License-Identifier: AGPL-3.0-only

// The server side of the replica: the three read routes WP-2.2a shipped.
//
// It goes through web/src/api/client.ts rather than calling fetch itself, which
// is ADR-017 rationale 1 made concrete — the core is written in the language of
// the seam it plugs into, so it reuses the one place this product talks to its
// server (cookie credentials, problem+json errors) instead of opening a second.

import { ApiError, request } from "../api/client.ts";
import type { FeedPage, ServerRecord } from "./core.ts";
import type { MetaObject } from "./schema.ts";
import { DeviceWipedError } from "./wipe.ts";

/** maxPage is the server's own cap (internal/app/sync.go maxFeedLimit). Asking
 * for more is a 400, so the client does not guess. */
export const MAX_PAGE = 1000;

/** One page of GET /api/v1/sync/snapshot. */
export interface SnapshotPage {
  object: string;
  data: ServerRecord[];
  /** The feed's high-water mark now. The client keeps the one from its *first*
   * page and resumes there (WP-2.2-decisions.md §4). */
  cursor: number;
  /** The id to page from, empty when this page is the last. */
  next: string;
}

/** One stored command, as the drain replays it. Structurally the subset of
 * `OutboxCommand` a request needs — declared here rather than imported so
 * outbox.ts depends on this file and not the other way round. */
export interface ReplayableCommand {
  commandId: string;
  method: string;
  path: string;
  body: Record<string, unknown> | null;
}

/** What replaying a command produced.
 *
 * A non-2xx is a *result*, not a thrown error: the drain has to read the status
 * and the problem+json to decide between retrying, filing a conflict, and
 * accepting. Throwing is reserved for "the request may or may not have been
 * received", which is the one case where the command must stay queued
 * (WP-2.3-decisions.md §1). */
export interface CommandResult {
  status: number;
  /** The parsed body: the record on success, the problem document on failure,
   * and null for a 204. */
  body: unknown;
}

/** Transport is the replica's whole dependency on the network. It is an
 * interface so the convergence harness can drive the core against a recorded
 * or mutated feed without a server, which is what makes §8's mutation check
 * possible. */
export interface Transport {
  meta(): Promise<MetaObject[]>;
  snapshot(object: string, after: string, limit: number): Promise<SnapshotPage>;
  changes(after: number, limit: number): Promise<FeedPage>;
  /** The scope keys this principal may replicate (WP-2.4). A list rather than a
   * version stamp: the client re-shapes by diffing it against what it holds, so
   * "did it change" and "to what" are one answer (WP-2.4-decisions.md §2). */
  scope(): Promise<string[]>;
  /** Replay one queued command. */
  command(command: ReplayableCommand): Promise<CommandResult>;
}

/** get is `request` with one translation: the wipe 401 becomes a
 * DeviceWipedError.
 *
 * It wraps every read rather than only the drain, because a wipe must be
 * honored at whichever request happens to be first on reconnect — the sync
 * cycle opens with `meta()`, so in practice that is usually a GET and not a
 * command at all (WP-2.5-decisions.md §2: the signal rides the auth path, so
 * *every* route carries it). */
async function get<T>(path: string): Promise<T> {
  try {
    return await request<T>(path);
  } catch (err) {
    if (err instanceof ApiError && err.isDeviceWiped) throw new DeviceWipedError();
    throw err;
  }
}

/** httpTransport talks to a live server. */
export function httpTransport(): Transport {
  return {
    async meta(): Promise<MetaObject[]> {
      const body = await get<{ data: MetaObject[] }>("/api/v1/meta/objects");
      return body.data;
    },

    async snapshot(object: string, after: string, limit: number): Promise<SnapshotPage> {
      const params = new URLSearchParams({ object, limit: String(limit) });
      if (after !== "") params.set("after", after);
      return get<SnapshotPage>(`/api/v1/sync/snapshot?${params}`);
    },

    async scope(): Promise<string[]> {
      const body = await get<{ data: string[] }>("/api/v1/sync/scope");
      return body.data;
    },

    async changes(after: number, limit: number): Promise<FeedPage> {
      const params = new URLSearchParams({
        after: String(after),
        limit: String(limit),
        include: "rows",
      });
      return get<FeedPage>(`/api/v1/sync/changes?${params}`);
    },

    // The whole of WP-2.3-decisions.md §1, in nine lines: a queued command is
    // replayed as the ordinary request it always was, through the same
    // `request()` every screen uses, at the same route, with the command_id as
    // the Idempotency-Key. There is no sync write endpoint to prefer, so INV-S2
    // ("no privileged sync side door") is not a property under test here — it
    // is a property there is no way to express a violation of.
    //
    // ApiError is unwrapped rather than propagated because a rejection is an
    // answer: the drain needs the status and the problem document to choose
    // between retrying, filing a conflict and accepting. A thrown error keeps
    // its meaning of "the request may not have arrived", which is the only case
    // that must leave the command queued.
    async command(command: ReplayableCommand): Promise<CommandResult> {
      try {
        const body = await request<unknown>(command.path, {
          method: command.method,
          body: command.body ?? undefined,
          idempotencyKey: command.commandId,
        });
        return { status: body === undefined ? 204 : 200, body: body ?? null };
      } catch (err) {
        // A wipe is not a rejection of *this* command, so it must not become
        // one. Returning it as a result would have the drain roll the row back
        // and file a conflict — bookkeeping written into a replica that is
        // about to be destroyed, and a tray entry nobody will ever read
        // (WP-2.5-decisions.md §6). It throws past the drain instead.
        if (err instanceof ApiError && err.isDeviceWiped) throw new DeviceWipedError();
        if (err instanceof ApiError) {
          return { status: err.problem.status, body: err.problem };
        }
        throw err;
      }
    },
  };
}

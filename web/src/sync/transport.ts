// SPDX-License-Identifier: AGPL-3.0-only

// The server side of the replica: the three read routes WP-2.2a shipped.
//
// It goes through web/src/api/client.ts rather than calling fetch itself, which
// is ADR-017 rationale 1 made concrete — the core is written in the language of
// the seam it plugs into, so it reuses the one place this product talks to its
// server (cookie credentials, problem+json errors) instead of opening a second.

import { request } from "../api/client.ts";
import type { FeedPage, ServerRecord } from "./core.ts";
import type { MetaObject } from "./schema.ts";

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

/** Transport is the replica's whole dependency on the network. It is an
 * interface so the convergence harness can drive the core against a recorded
 * or mutated feed without a server, which is what makes §8's mutation check
 * possible. */
export interface Transport {
  meta(): Promise<MetaObject[]>;
  snapshot(object: string, after: string, limit: number): Promise<SnapshotPage>;
  changes(after: number, limit: number): Promise<FeedPage>;
}

/** httpTransport talks to a live server. */
export function httpTransport(): Transport {
  return {
    async meta(): Promise<MetaObject[]> {
      const body = await request<{ data: MetaObject[] }>("/api/v1/meta/objects");
      return body.data;
    },

    async snapshot(object: string, after: string, limit: number): Promise<SnapshotPage> {
      const params = new URLSearchParams({ object, limit: String(limit) });
      if (after !== "") params.set("after", after);
      return request<SnapshotPage>(`/api/v1/sync/snapshot?${params}`);
    },

    async changes(after: number, limit: number): Promise<FeedPage> {
      const params = new URLSearchParams({
        after: String(after),
        limit: String(limit),
        include: "rows",
      });
      return request<FeedPage>(`/api/v1/sync/changes?${params}`);
    },
  };
}

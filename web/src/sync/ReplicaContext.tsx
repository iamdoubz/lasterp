// SPDX-License-Identifier: AGPL-3.0-only

// The replica, as the React tree sees it.
//
// One worker per tab, started lazily on first use rather than at mount. That is
// not an optimisation: starting it eagerly would make every page load take an
// exclusive OPFS handle, and a second tab — which for an ERP user is ordinary —
// would find the replica locked before anyone had asked it for anything
// (WP-2.2b-decisions.md §5). Started on demand, the failure lands on the screen
// that needs it, where there is something honest to render.

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

import { startReplica, type SyncClient } from "./host.ts";
import type { SyncStatus } from "./protocol.ts";

interface ReplicaValue {
  client: SyncClient;
  /** Whether anything has actually opened the replica yet. */
  started: boolean;
}

const ReplicaContext = createContext<ReplicaValue | null>(null);

/** ReplicaProvider hands one SyncClient to the tree. `client` is injectable so
 * screens can be tested without a worker: the tray's behaviour is about what it
 * does with conflicts, not about postMessage. */
export function ReplicaProvider({
  children,
  client,
}: {
  children: ReactNode;
  client?: SyncClient;
}) {
  const [started, setStarted] = useState(client !== undefined);
  const instance = useMemo(() => client ?? lazyClient(() => setStarted(true)), [client]);
  const value = useMemo(() => ({ client: instance, started }), [instance, started]);
  return <ReplicaContext.Provider value={value}>{children}</ReplicaContext.Provider>;
}

function useReplicaValue(): ReplicaValue {
  const value = useContext(ReplicaContext);
  if (value === null) {
    throw new Error("sync: useReplica outside a ReplicaProvider");
  }
  return value;
}

export function useReplica(): SyncClient {
  return useReplicaValue().client;
}

/** lazyClient defers `startReplica` to the first call on it.
 *
 * A SyncClient is six async methods and a close, so the laziness is a
 * pass-through per method rather than a wrapper type — and `close` deliberately
 * does *not* start a worker in order to shut one down.
 *
 * Laziness is load-bearing, not tidiness. Opening the replica takes an
 * *exclusive* OPFS access handle, so a provider that started on mount would
 * claim it on every page load — and the second tab, which for an ERP user is
 * ordinary, would find the replica locked before anyone had asked it for
 * anything (WP-2.2b-decisions.md §5). Started on demand, the cost is paid by
 * the screen that wanted it, where there is something honest to render. */
function lazyClient(onStart: () => void): SyncClient {
  let started: SyncClient | undefined;
  const client = () => {
    if (started === undefined) {
      started = startReplica();
      onStart();
    }
    return started;
  };

  return {
    sync: () => client().sync(),
    status: () => client().status(),
    list: (object) => client().list(object),
    write: (command) => client().write(command),
    conflicts: () => client().conflicts(),
    discard: (commandId) => client().discard(commandId),
    close: () => started?.close(),
  };
}

/** SYNC_STATUS_KEY is the query the shell's indicator and the tray both read,
 * so draining invalidates one thing and both update. */
export const SYNC_STATUS_KEY = ["sync", "status"] as const;
export const SYNC_CONFLICTS_KEY = ["sync", "conflicts"] as const;

/** useSyncStatus reads the replica's own account of itself.
 *
 * Failures resolve to null rather than propagating: a replica held by another
 * tab is a *state*, and a shell indicator is not the place to render it — the
 * screens that act on the replica surface it where there is something to say.
 */
export function useSyncStatus(): SyncStatus | null {
  const { client, started } = useReplicaValue();
  const { data } = useQuery({
    queryKey: SYNC_STATUS_KEY,
    queryFn: () => client.status().catch(() => null),
    // Only once something has opened the replica. A status indicator must not
    // be the thing that brings a replica into existence: it would take the
    // exclusive OPFS handle on every page load, on behalf of a badge that has
    // nothing to report until there is queued work — and until the shipped
    // screens write through the outbox (WP-2.3-decisions.md §13) there is none.
    enabled: started,
    // The replica is local; asking it is a SQLite count, not a round trip.
    staleTime: 1_000,
    retry: false,
  });
  return data ?? null;
}

/** useDrainOnReconnect sends queued work at the two moments the window in which
 * it lives in exactly one place can be closed: the network coming back, and the
 * user returning to the tab.
 *
 * docs/04's live transport does not exist yet (WP-2.1 §7), so the alternative
 * is waiting for a poll tick — which is a stretch of time where the only copy
 * of the user's work is in a browser store the browser may evict
 * (WP-2.3-decisions.md §6.3). */
export function useDrainOnReconnect(): void {
  const { client, started } = useReplicaValue();
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!started) return;
    let stopped = false;
    const drain = () => {
      if (stopped || document.visibilityState === "hidden" || !navigator.onLine) return;
      void client.sync().then(
        () => {
          void queryClient.invalidateQueries({ queryKey: ["sync"] });
        },
        // Offline again, or a locked replica. Both are ordinary and both are
        // reported by the screens that care; a background drain has no user
        // waiting on it and nothing to add by throwing.
        () => undefined,
      );
    };

    addEventListener("online", drain);
    document.addEventListener("visibilitychange", drain);
    return () => {
      stopped = true;
      removeEventListener("online", drain);
      document.removeEventListener("visibilitychange", drain);
    };
  }, [client, queryClient, started]);
}

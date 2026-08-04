// SPDX-License-Identifier: AGPL-3.0-only

// A second browsing context competing for the same replica.
//
// The SAH pool takes its access handles **exclusively**, so this must fail —
// and it must fail as ReplicaLockedError, which is the distinct type the shell
// renders as "open in another tab" rather than as a blank screen. An ERP user
// with two tabs open is ordinary, and before WP-2.2b nothing in the design had
// modelled it (WP-2.2b-decisions.md §5).
//
// Coordination — Web Locks plus a shared worker proxying the replica — is
// WP-2.3's, where two writers stop being a usability question and become a
// correctness one.

import { openOpfsStore, ReplicaLockedError } from "../src/sync/adapters/opfs.ts";

openOpfsStore("converge.sqlite3")
  .then((store) => {
    store.close();
    postMessage({ refused: false, error: "" });
  })
  .catch((err: unknown) => {
    postMessage({
      refused: err instanceof ReplicaLockedError,
      error: err instanceof Error ? `${err.name}: ${err.message}` : String(err),
    });
  });

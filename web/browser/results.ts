// SPDX-License-Identifier: AGPL-3.0-only

/** What the browser harness reports back to the page and the Playwright spec. */
export interface BrowserResults {
  /** The shared conformance suite (src/sync/suite.ts), run over OPFS. */
  suite: { name: string; ok: boolean; error?: string }[];

  /** Convergence of a full hydrate + apply cycle, over OPFS in a worker. */
  converged?: { rows: number; cursor: number; names: string[] };

  /** ADR-017 §Consequences left the SAH pool's slot count to WP-2.2. This is
   * the measurement that keeps the chosen number honest. */
  pool?: { capacity: number; filesInUse: number };

  /** Whether a second worker holding the same replica was refused with the
   * distinct error the shell renders as a state (WP-2.2b-decisions.md §5). */
  secondTab?: { refused: boolean; error: string };

  /** Throughput, carried forward from the WP-2.6 spike so a regression in the
   * apply path is visible rather than merely absent. */
  bench?: { changes: number; ms: number; perSecond: number; rows: number };

  fatal?: string;
}

declare global {
  interface Window {
    __replica?: BrowserResults;
  }
}

// SPDX-License-Identifier: AGPL-3.0-only

/** What the spike worker reports back to the page and the Playwright spec. */
export interface SpikeResults {
  suite: { name: string; ok: boolean; error?: string }[];
  bench?: { changes: number; ms: number; perSecond: number; rows: number };
  fatal?: string;
}

declare global {
  interface Window {
    __spike?: SpikeResults;
  }
}

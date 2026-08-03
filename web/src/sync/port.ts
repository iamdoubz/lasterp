// SPDX-License-Identifier: AGPL-3.0-only

// The driver port. Everything the sync core needs from a SQLite, and nothing
// else — one adapter per platform implements it (WP-2.6 bar 1: the core must
// carry no platform branches).
//
// Synchronous on purpose. Both drivers can be: node:sqlite's DatabaseSync is
// synchronous, and SQLite-WASM's OPFS SAH-pool VFS is synchronous once opened.
// An async port would force every caller into promises to serve a constraint
// neither driver actually has, and would make the transaction boundary — the
// thing INV-S5 depends on — something you can accidentally await across.

/** A value SQLite can bind. */
export type SqlValue = string | number | bigint | null;

/** Store is one open replica database. */
export interface Store {
  /** Run a statement for effect. */
  exec(sql: string, params?: SqlValue[]): void;

  /** Run a query and return all rows. */
  query<T = Record<string, SqlValue>>(sql: string, params?: SqlValue[]): T[];

  /**
   * Run fn inside a transaction, committing on return and rolling back if it
   * throws. The rollback is what makes cursor advancement crash-safe: the
   * cursor is written inside the same transaction as the rows it accounts for,
   * so there is no window where one landed and the other did not.
   */
  transaction<T>(fn: () => T): T;

  close(): void;
}

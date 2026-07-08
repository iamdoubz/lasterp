// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite is the SQLite storage.DB adapter (ADR-002): the embedded
// store for solo mode and every client replica. Uses modernc.org/sqlite —
// pure Go, no CGO, per CLAUDE.md.
package sqlite

import (
	_ "modernc.org/sqlite"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// Open opens a SQLite-backed storage.DB at path (use ":memory:" or a
// file:...?mode=memory&cache=shared DSN for ephemeral use).
func Open(path string) (*storage.DB, error) {
	return storage.Open(storage.SQLite, "sqlite", path)
}

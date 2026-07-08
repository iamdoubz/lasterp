// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres is the Postgres storage.DB adapter (ADR-002, ADR-015).
// It uses pgx's database/sql driver in stdlib mode — pure Go, no CGO,
// consistent with CLAUDE.md's no-CGO rule.
package postgres

import (
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// Open opens a Postgres-backed storage.DB. dsn is a standard Postgres
// connection string (e.g. "postgres://user:pass@host:5432/db").
func Open(dsn string) (*storage.DB, error) {
	return storage.Open(storage.Postgres, "pgx", dsn)
}

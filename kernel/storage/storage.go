// SPDX-License-Identifier: AGPL-3.0-only

// Package storage is the thin adapter layer over database/sql that covers
// the small SQL dialect gap between Postgres and SQLite (ADR-002, ADR-015).
// It deliberately does not wrap database/sql's query surface: DB embeds
// *sql.DB directly so callers use the standard library API unmodified.
package storage

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Dialect identifies which SQL engine a DB talks to.
type Dialect int

const (
	Postgres Dialect = iota
	SQLite
)

func (d Dialect) String() string {
	switch d {
	case Postgres:
		return "postgres"
	case SQLite:
		return "sqlite"
	default:
		return "unknown"
	}
}

// DB is a dialect-tagged database/sql handle.
type DB struct {
	*sql.DB
	Dialect Dialect
}

// Open opens a connection pool for the given dialect and driver-specific
// DSN. driverName must already be registered (pgx's stdlib driver as
// "pgx", modernc.org/sqlite as "sqlite").
func Open(dialect Dialect, driverName, dsn string) (*DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", dialect, err)
	}
	return &DB{DB: db, Dialect: dialect}, nil
}

// Rebind rewrites a query written with SQLite-style "?" placeholders into
// the target dialect's placeholder syntax. Postgres uses positional "$1",
// "$2", ...; SQLite queries pass through unchanged. Write queries with "?"
// placeholders and call Rebind before executing on a Postgres DB.
//
// Rebind does a naive byte-scan substitution with no awareness of quoted
// string literals: a query containing a literal "?" inside a string value
// (e.g. a LIKE pattern) will be mis-rewritten on Postgres. Bind such values
// as parameters instead of embedding them in the query text.
func (d *DB) Rebind(query string) string {
	if d.Dialect != Postgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

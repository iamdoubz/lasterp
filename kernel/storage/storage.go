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
	"time"
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

// sqliteTimeFormat matches modernc.org/sqlite's default write format
// (time.Time.String(), unless a DSN _time_format overrides it — this
// project doesn't set one).
const sqliteTimeFormat = "2006-01-02 15:04:05.999999999 -0700 MST"

// Time scans a TIMESTAMPTZ column into a time.Time, working around
// modernc.org/sqlite: it only auto-parses TEXT timestamp columns declared
// exactly DATE/DATETIME/TIMESTAMP, not TIMESTAMPTZ — the type this project
// uses on Postgres (docs/notes/WP-0.2-decisions.md), so SQLite hands back
// a raw string instead of a time.Time. Use *Time as the Scan destination
// for any TIMESTAMPTZ column; pass a plain time.Time for Exec/Query args.
type Time struct{ time.Time }

func (t *Time) Scan(v any) error {
	switch val := v.(type) {
	case time.Time:
		t.Time = val
	case string:
		parsed, err := time.Parse(sqliteTimeFormat, val)
		if err != nil {
			return fmt.Errorf("storage: parse time %q: %w", val, err)
		}
		t.Time = parsed
	case []byte:
		return t.Scan(string(val))
	case nil:
		t.Time = time.Time{}
	default:
		return fmt.Errorf("storage: unsupported time value type %T", v)
	}
	return nil
}

// NullTime is Time's nullable counterpart, for TIMESTAMPTZ columns that
// allow NULL.
type NullTime struct {
	Time  time.Time
	Valid bool
}

func (n *NullTime) Scan(v any) error {
	if v == nil {
		n.Time, n.Valid = time.Time{}, false
		return nil
	}
	var t Time
	if err := t.Scan(v); err != nil {
		return err
	}
	n.Time, n.Valid = t.Time, true
	return nil
}

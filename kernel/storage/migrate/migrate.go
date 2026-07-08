// SPDX-License-Identifier: AGPL-3.0-only

// Package migrate is a minimal, hand-rolled migration runner: embedded .sql
// files applied forward-only in filename order, tracked in a
// schema_migrations bookkeeping table. Migrations follow the expand →
// backfill → contract discipline (docs/03-DATA-MODEL.md) by convention and
// PR review; this package does not detect destructive DDL.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migration is one forward SQL step, named after its source file.
type Migration struct {
	Version string // filename without extension/dialect tag, e.g. "0001_conformance_fixture"
	SQL     string
}

// Load reads and sorts the embedded migrations by version, selecting the
// dialect-specific variant of each version when one exists.
//
// A version's SQL comes from one of two file shapes: a dialect-neutral
// "NNNN_name.sql" (used for both dialects), or a pair of dialect-tagged
// files "NNNN_name.postgres.sql" / "NNNN_name.sqlite.sql" for steps whose
// syntax genuinely diverges (e.g. Postgres RLS policies). A version must
// not mix a neutral file with a tagged one.
func Load(dialect storage.Dialect) ([]Migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migrations dir: %w", err)
	}

	type candidate struct {
		neutral, dialectSpecific string // file names
	}
	byVersion := make(map[string]*candidate)
	var order []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sql")
		version := name
		tag := ""
		if i := strings.LastIndex(name, "."); i >= 0 {
			tag = name[i+1:]
			if tag == "postgres" || tag == "sqlite" {
				version = name[:i]
			} else {
				tag = ""
			}
		}

		c, ok := byVersion[version]
		if !ok {
			c = &candidate{}
			byVersion[version] = c
			order = append(order, version)
		}
		switch {
		case tag == "":
			c.neutral = e.Name()
		case tag == dialect.String():
			c.dialectSpecific = e.Name()
		}
	}

	sort.Strings(order)
	migrations := make([]Migration, 0, len(order))
	for _, version := range order {
		c := byVersion[version]
		file := c.dialectSpecific
		if file == "" {
			file = c.neutral
		}
		if file == "" {
			return nil, fmt.Errorf("migrate: version %s has no %s or neutral variant", version, dialect)
		}
		b, err := migrationsFS.ReadFile("migrations/" + file)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", file, err)
		}
		migrations = append(migrations, Migration{Version: version, SQL: string(b)})
	}
	return migrations, nil
}

// Apply runs every embedded migration not yet recorded in
// schema_migrations, in order, each in its own transaction. Re-running
// Apply against an up-to-date database is a no-op.
func Apply(ctx context.Context, db *storage.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}

	migrations, err := Load(db.Dialect)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		applied, err := isApplied(ctx, db, m.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", m.Version, err)
		}
	}
	return nil
}

func isApplied(ctx context.Context, db *storage.DB, version string) (bool, error) {
	var n int
	row := db.QueryRowContext(ctx, db.Rebind(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`), version)
	if err := row.Scan(&n); err != nil {
		return false, fmt.Errorf("migrate: check %s: %w", version, err)
	}
	return n > 0, nil
}

func applyOne(ctx context.Context, db *storage.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	// Bind an explicit UTC instant rather than relying on CURRENT_TIMESTAMP:
	// on Postgres, assigning to a TIMESTAMPTZ column preserves the instant
	// regardless of session time zone (CLAUDE.md: "Time: UTC in storage,
	// always"), whereas a bare SQL CURRENT_TIMESTAMP cast into a
	// TIMESTAMP-without-time-zone column would be session-timezone-dependent.
	if _, err := tx.ExecContext(ctx, db.Rebind(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), m.Version, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

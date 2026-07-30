// SPDX-License-Identifier: AGPL-3.0-only

// Package app is the LastERP composition root: it wires the kernel gateway to
// the built-in modules to produce the real product HTTP surface (WP-1.4b). It
// is neither kernel nor a module, so it may import both — this is the one place
// module wiring is allowed to cross the layering boundary (CLAUDE.md). Before
// WP-1.4b, `lasterp serve` booted the zero-config hello-world handler and every
// Phase-1 capability lived only inside test harnesses; this package makes them
// reachable over API/MCP (the "everything is an API" commandment).
package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/modules/contacts"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

// Open opens the database named by dsn, runs all migrations, registers every
// built-in module's schema, and loads the tax seed packs. A dsn beginning
// "postgres://"/"postgresql://" uses the Postgres adapter; anything else (or
// empty) is a SQLite path (default "lasterp.db"). The caller owns Close.
//
// Modules are registered unconditionally: Register creates global schema
// (tables, triggers), while capability enable-state is per-tenant and gated at
// request time by the GatewayChecker (WP-1.4b-decisions.md §6). Database role
// separation (the REVOKEs on the app role) is a deployment step, not a boot
// step (§7) — the posting happy-path works through the pipeline functions the
// migrations create regardless.
func Open(ctx context.Context, dsn string) (*storage.DB, error) {
	db, err := openDialect(dsn)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := Setup(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies all schema migrations. On Postgres it must run under a
// privileged role: the migrations create the SECURITY DEFINER pipeline
// functions (append_event, ledger_post_entry) that let the locked-down app
// role write the append-only log only through the pipeline (INV-F5). Splitting
// Migrate from Setup lets a deployment run migrations under a bootstrap role
// and serve under a restricted app role (proven by the boot e2e).
func Migrate(ctx context.Context, db *storage.DB) error {
	if err := migrate.Apply(ctx, db); err != nil {
		return fmt.Errorf("app: migrate: %w", err)
	}
	return nil
}

// Setup registers every built-in module's schema and loads the tax seed packs.
// It may run under the restricted app role (which owns the obj_* tables it
// creates via ApplyDDL).
func Setup(ctx context.Context, db *storage.DB) error {
	if err := registerModules(ctx, db); err != nil {
		return err
	}
	if err := tax.LoadSeedPacks(ctx, db); err != nil {
		return fmt.Errorf("app: load tax seed packs: %w", err)
	}
	return nil
}

func openDialect(dsn string) (*storage.DB, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return postgres.Open(dsn)
	}
	if dsn == "" {
		dsn = "lasterp.db"
	}
	return sqlite.Open(dsn)
}

// registerModules registers every built-in module's schema. Order matters only
// in that a module referencing another's object by id needs no import; there
// are no cross-module DDL dependencies here. Tax owns raw reference tables (no
// metadata objects), so it has no Register.
func registerModules(ctx context.Context, db *storage.DB) error {
	regs := []struct {
		name string
		fn   func(context.Context, *storage.DB) error
	}{
		{"contacts", contacts.Register},
		{"ledger", ledger.Register},
		{"invoicing", invoicing.Register},
	}
	for _, r := range regs {
		if err := r.fn(ctx, db); err != nil {
			return fmt.Errorf("app: register %s: %w", r.name, err)
		}
	}
	return nil
}

// Handler builds the fully-wired product API handler over an already-opened,
// migrated db (see Open). It exposes the CRUD-safe objects (Account, Contact),
// the non-CRUD action surface (actions.go), the bearer-session authenticator,
// and per-tenant capability gating.
func Handler(db *storage.DB) (http.Handler, error) {
	reg, err := capability.Load()
	if err != nil {
		return nil, fmt.Errorf("app: load capability registry: %w", err)
	}
	objects, err := crudObjects()
	if err != nil {
		return nil, err
	}
	return api.NewGateway(api.Config{
		DB:            db,
		Objects:       objects,
		Actions:       actions(db, reg),
		Authenticator: sessionAuthenticator(db),
		Capabilities:  capability.GatewayChecker{Reg: reg, DB: db},
	}), nil
}

// crudObjects are the objects safe to expose as full generic CRUD. Financial
// documents (Invoice) and monotonic-state objects (Period) are deliberately
// excluded — they get bespoke action routes instead (WP-1.4b-decisions.md §2).
func crudObjects() ([]*metadata.EffectiveSchema, error) {
	account, err := ledger.AccountSchema()
	if err != nil {
		return nil, fmt.Errorf("app: account schema: %w", err)
	}
	contact, err := contacts.ContactSchema()
	if err != nil {
		return nil, fmt.Errorf("app: contact schema: %w", err)
	}
	return []*metadata.EffectiveSchema{account, contact}, nil
}

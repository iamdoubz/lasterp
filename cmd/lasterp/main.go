// SPDX-License-Identifier: AGPL-3.0-only

// Command lasterp is the LastERP CLI: `serve` runs the product API and the
// built web client, `dev` additionally launches the web dev server for local
// iteration, `bootstrap` provisions the first tenant and administrator, and
// `demo` fills a tenant with a small book so the dashboards have something real
// to show.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/iamdoubz/lasterp/internal/app"
)

const usage = "usage: lasterp <serve|dev|bootstrap|demo>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		if err := serve(context.Background()); err != nil {
			log.Fatal(err)
		}
	case "dev":
		if err := dev(context.Background()); err != nil {
			log.Fatal(err)
		}
	case "bootstrap":
		if err := bootstrap(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "demo":
		if err := demo(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; %s\n", os.Args[1], usage)
		os.Exit(1)
	}
}

// demo seeds an existing tenant with a small demo book — a chart of accounts,
// two fiscal periods, customers, posted invoices and a receipt — so a fresh
// deployment shows live dashboards instead of a grid of zeroes.
//
// Every write goes through the same authorized module entry points the API uses
// (INV-X5: bulk paths get batching, not bypasses), and it refuses to run against
// a tenant that already has posted invoices.
func demo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant id to seed")
	email := fs.String("email", "", "existing user the seeded writes are attributed to")
	currency := fs.String("currency", "EUR", "currency the demo book is kept in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := app.Open(ctx, os.Getenv("LASTERP_DSN"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := app.SeedDemo(ctx, db, app.DemoInput{
		Tenant: *tenant, Email: *email, Currency: *currency,
	}); err != nil {
		return err
	}
	log.Printf("seeded demo book for tenant %q", *tenant)
	return nil
}

// bootstrap provisions the first tenant and administrator. A freshly migrated
// database has no tenants and no users, and tenant creation is deliberately not
// an API — so this is the only way into a new deployment.
func bootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant id (also the tenant field on the sign-in form)")
	name := fs.String("name", "", "human-readable tenant name (defaults to the tenant id)")
	email := fs.String("email", "", "administrator email")
	password := fs.String("password", "", "administrator password (prefer LASTERP_BOOTSTRAP_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// A password on the command line lands in shell history and the process
	// table, so the environment variable wins when both are set.
	pw := os.Getenv("LASTERP_BOOTSTRAP_PASSWORD")
	if pw == "" {
		pw = *password
	}

	db, err := app.Open(ctx, os.Getenv("LASTERP_DSN"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := app.Bootstrap(ctx, db, app.BootstrapInput{
		Tenant: *tenant, Name: *name, Email: *email, Password: pw,
	}); err != nil {
		return err
	}
	log.Printf("bootstrapped tenant %q with administrator %s", *tenant, *email)
	return nil
}

// addr is the listen address, overridable with LASTERP_ADDR (default :8080).
func addr() string {
	if a := os.Getenv("LASTERP_ADDR"); a != "" {
		return a
	}
	return ":8080"
}

// buildHandler opens the database (LASTERP_DSN — Postgres URL or SQLite path,
// default lasterp.db), migrates it, registers the modules, and returns the
// fully-wired product API handler.
func buildHandler(ctx context.Context) (http.Handler, error) {
	db, err := app.Open(ctx, os.Getenv("LASTERP_DSN"))
	if err != nil {
		return nil, err
	}
	return app.Handler(ctx, db)
}

func serve(ctx context.Context) error {
	handler, err := buildHandler(ctx)
	if err != nil {
		return err
	}
	listen := addr()
	srv := &http.Server{Addr: listen, Handler: handler}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("LastERP API listening on %s", listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// dev starts the API in the background and runs the web dev server in the
// foreground; Ctrl+C stops both.
func dev(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler, err := buildHandler(ctx)
	if err != nil {
		return err
	}
	listen := addr()
	srv := &http.Server{Addr: listen, Handler: handler}
	go func() {
		log.Printf("LastERP API listening on %s", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("API stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	web := exec.CommandContext(ctx, "pnpm", "--dir", "web", "run", "dev")
	web.Stdout, web.Stderr, web.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := web.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("web dev server: %w", err)
	}
	return nil
}

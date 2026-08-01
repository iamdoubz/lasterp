// SPDX-License-Identifier: AGPL-3.0-only

// Command lasterp is the LastERP CLI.
//
// Running it: `serve` runs the product API and the built web client, `dev`
// additionally launches the web dev server for local iteration.
//
// Deploying it: `migrate` applies schema migrations as a privileged role,
// `harden` locks the application role down to the write pipeline (docs/19 §2),
// and `doctor` reports whether a running deployment is actually separated.
//
// Filling it: `bootstrap` provisions the first tenant and administrator, and
// `demo` seeds a small book so the dashboards have something real to show.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/iamdoubz/lasterp/internal/app"
	"github.com/iamdoubz/lasterp/kernel/storage"
)

const usage = "usage: lasterp <serve|dev|migrate|harden|doctor|bootstrap|demo>"

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
	case "migrate":
		if err := migrate(context.Background()); err != nil {
			log.Fatal(err)
		}
	case "harden":
		if err := harden(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "doctor":
		if err := doctor(context.Background()); err != nil {
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

// migrate applies schema migrations and nothing else.
//
// app.Migrate has been split from app.Setup since WP-1.4b precisely so a
// deployment can migrate as a privileged role and serve as a restricted one —
// the SECURITY DEFINER pipeline functions have to be owned by a role that keeps
// INSERT on events, which is exactly the privilege the serving role gives up.
// Until WP-1.10 there was no way to invoke that split from outside the process,
// so the two-role deployment it enables could not actually be deployed.
//
// `serve` still migrates on boot, which keeps solo mode a single command; when
// the schema is already current that is a no-op.
func migrate(ctx context.Context) error {
	db, err := Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := app.Migrate(ctx, db); err != nil {
		return err
	}
	log.Print("schema migrations applied")
	return nil
}

// harden provisions the restricted application role and applies the docs/19
// role-separation grants: the app can append to the event log only through the
// pipeline functions, and cannot mutate the append-only tables at all.
//
// It connects with LASTERP_DSN, which for this command must be an *owner*
// connection (the role that ran the migrations, or a superuser) — it is
// administering grants on someone else's behalf. The role it hardens is a
// different, restricted one, and is what `lasterp serve` should connect as.
//
// Idempotent on purpose: a deployment runs this on every boot rather than
// remembering whether it is the first.
func harden(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("harden", flag.ExitOnError)
	role := fs.String("app-role", "lasterp_app", "restricted role the application serves as")
	create := fs.Bool("create-role", false, "create the role if it does not exist (password from LASTERP_APP_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if *create {
		// Same reasoning as bootstrap: a password on the command line lands in
		// shell history and the process table, so it only comes from the
		// environment.
		if err := app.ProvisionAppRole(ctx, db, *role, os.Getenv("LASTERP_APP_PASSWORD")); err != nil {
			return err
		}
		log.Printf("provisioned application role %q", *role)
	}
	if err := app.Harden(ctx, db, *role); err != nil {
		return err
	}
	log.Printf("applied role separation to %q", *role)
	return nil
}

// doctor reports whether this deployment is actually running with the storage
// guarantees the Integrity Gauntlet proves. It connects as the application does
// — LASTERP_DSN unchanged — because the question is about that connection.
//
// Exit status is the point: non-zero when the deployment is not separated, so it
// can be a readiness gate or a deploy check rather than something a human has to
// read and interpret.
func doctor(ctx context.Context) error {
	db, err := Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	p, err := app.Diagnose(ctx, db)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))

	if !p.Separated {
		return fmt.Errorf("database role separation is not in effect (%d finding(s)); run `lasterp harden`", len(p.Findings))
	}
	return nil
}

// Open opens the database named by LASTERP_DSN without running migrations or
// module registration. harden and doctor both administer or inspect an existing
// database rather than bringing one up.
func Open(ctx context.Context) (*storage.DB, error) {
	return app.OpenRaw(ctx, os.Getenv("LASTERP_DSN"))
}

// newServer builds the HTTP server with timeouts on every phase of a request.
// A server with none — which is Go's zero value, and was this product's
// configuration until WP-1.10 — lets a client hold a connection open forever by
// dribbling a request header one byte at a time, exhausting the accept queue
// from a single host (Slowloris, gosec G112).
//
// ReadHeaderTimeout is the one that closes that specific hole; the rest bound
// the phases after it. WriteTimeout is the loosest of the four because a report
// export legitimately takes longer to produce than an interactive read, and a
// budget that kills a large XLSX mid-stream would be a worse bug than the one
// being fixed (docs/09 budgets are p95 targets for interactive paths, not caps
// on every response).
func newServer(listen string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
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
	srv := newServer(listen, handler)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown(srv)
	}()

	log.Printf("LastERP API listening on %s", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// drainTimeout bounds how long a shutdown waits for in-flight requests.
const drainTimeout = 20 * time.Second

// shutdown stops the server gracefully: stop accepting, let in-flight requests
// finish, then close. Until WP-1.10 this was srv.Close(), which severs live
// connections immediately — so every deploy and every SIGTERM in a rolling
// restart cut off whatever was mid-flight. Nothing could be *corrupted* by that
// (writes are transactional and the ledger is append-only), but a user watching
// an invoice post saw a network error and had no way to know whether it landed,
// which is its own kind of harm in an accounting system.
//
// The drain is bounded: a request wedged behind a slow query must not hold a
// deployment open indefinitely, and Kubernetes will SIGKILL us anyway once its
// grace period expires.
func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		// The drain window closed with requests still running. Say so — a
		// deployment that routinely trips this is a deployment with a slow path
		// nobody has measured.
		log.Printf("shutdown: drain incomplete after %s: %v", drainTimeout, err)
		_ = srv.Close()
	}
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
	srv := newServer(listen, handler)
	go func() {
		log.Printf("LastERP API listening on %s", listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("API stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown(srv)
	}()

	web := exec.CommandContext(ctx, "pnpm", "--dir", "web", "run", "dev")
	web.Stdout, web.Stderr, web.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := web.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("web dev server: %w", err)
	}
	return nil
}

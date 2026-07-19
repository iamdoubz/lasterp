// SPDX-License-Identifier: AGPL-3.0-only

// Command lasterp is the LastERP CLI: `serve` runs the kernel API alone,
// `dev` additionally launches the web dev server for local iteration.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/iamdoubz/lasterp/internal/app"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lasterp <serve|dev>")
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
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; usage: lasterp <serve|dev>\n", os.Args[1])
		os.Exit(1)
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
	return app.Handler(db)
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

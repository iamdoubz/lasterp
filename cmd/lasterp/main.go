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

	"github.com/iamdoubz/lasterp/kernel/api"
)

const apiAddr = ":8080"

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

func serve(ctx context.Context) error {
	srv := &http.Server{Addr: apiAddr, Handler: api.NewMux()}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("kernel API listening on %s", apiAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// dev starts the kernel API in the background and runs the web dev
// server in the foreground; Ctrl+C stops both.
func dev(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: apiAddr, Handler: api.NewMux()}
	go func() {
		log.Printf("kernel API listening on %s", apiAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("kernel API stopped: %v", err)
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

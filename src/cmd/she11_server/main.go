// Command she11_server is the public relay.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"she11/src/server"
	"she11/src/shared/lock"
	"she11/src/shared/transport"
)

// mutexName is injected at build time by builder.py (--mutex).
var mutexName string

func main() {
	cfg, err := server.ParseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "she11_server:", err)
		os.Exit(2)
	}
	if cfg == nil { // help / version printed
		os.Exit(0)
	}

	release, err := lock.Acquire(mutexName)
	if err == lock.ErrHeld {
		fmt.Fprintln(os.Stderr, "she11_server: another instance is already running (mutex)")
		os.Exit(9)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "she11_server: mutex:", err)
		os.Exit(2)
	}
	defer release()

	tlsCfg, err := transport.ServerTLS(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "she11_server:", err)
		os.Exit(2)
	}

	logger := server.NewLogger(cfg.Log.Folder, cfg.Log.Level)
	srv := server.New(*cfg, tlsCfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "she11_server:", err)
		os.Exit(2)
	}
	logger.Printf("shutdown complete")
}

// Command bitblocker is the BitBlocker daemon entry point.
//
// It wires configuration, logging, the blocklist cache, the data
// fetcher, and the forwardAuth HTTP server. See docs/BitBlocker.md for
// the product overview and docs/bitblocker-spec.md for the component
// specification.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/logging"
	"github.com/bitsalt/bitblocker/internal/server"
)

const exitFailure = 1

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "bitblocker: %v\n", err)
		os.Exit(exitFailure)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("bitblocker", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/bitblocker/config.yaml", "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}

	logger.Info("bitblocker: starting",
		"listen", server.AddrFromConfig(cfg.Listen),
		"countries", cfg.Block.Countries,
		"startup_mode", cfg.Behavior.StartupMode,
	)

	// The atomic-swap slice (separate Sprint 2 task) supplies the
	// real LookupSource backed by an atomic.Pointer[blocklist.Trie].
	// Until that lands, the server runs with an always-empty source:
	// /healthz returns 503 and /check fails closed. That is the
	// spec's defined cold-start posture, so the daemon is correct
	// (if not yet useful) end-to-end.
	emptyLookup := func() server.Lookup { return nil }

	srv, err := server.New(server.Options{
		Addr:        server.AddrFromConfig(cfg.Listen),
		Lookup:      emptyLookup,
		Logger:      logger,
		BlockStatus: cfg.Behavior.ResponseCode,
		LogBlocked:  cfg.Behavior.LogBlocked,
		LogAllowed:  cfg.Behavior.LogAllowed,
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}

	logger.Info("bitblocker: stopped")
	return nil
}

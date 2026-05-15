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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/diskcache"
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

	// The blocklist Source holds the active trie behind an atomic
	// pointer; the Sprint 3 fetcher/scheduler will Swap a fresh trie
	// in on every successful refresh. The disk cache below gives the
	// daemon a head start ahead of that first fetch.
	src := blocklist.NewSource()
	loadDiskCache(logger, src, cfg)

	srv, err := server.New(server.Options{
		Addr:        server.AddrFromConfig(cfg.Listen),
		Lookup:      newLookupSource(src),
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

// newLookupSource adapts a *blocklist.Source to the server's
// LookupSource seam.
//
// Hard requirement: the closure returns an UNTYPED nil when no trie is
// loaded. Returning a server.Lookup that wraps a (*blocklist.Trie)(nil)
// would be a non-nil interface holding a nil pointer — it passes the
// server's `lookup == nil` guard and then panics on lookup.Len(). The
// `if t != nil` guard is the regression fix for that trap (ADR 0001
// §"nil-pointer return", interface spec §2.5).
func newLookupSource(src *blocklist.Source) server.LookupSource {
	return func() server.Lookup {
		t := src.Current()
		if t == nil {
			// Untyped nil — see the contract note above. Returning
			// `t` here would wrap a nil *Trie in a non-nil interface.
			return nil
		}
		return t
	}
}

// loadDiskCache attempts to populate src from the on-disk blocklist
// snapshot before the first network fetch. Every failure mode is
// non-fatal: a missing, stale, or corrupt cache leaves the daemon in
// its documented fail-closed cold-start posture, exactly where it
// would be with no cache at all. See ADR 0002 §C.
func loadDiskCache(logger *slog.Logger, src *blocklist.Source, cfg *config.Config) {
	trie, err := diskcache.Load(cfg.Cache.Path, cfg.Cache.MaxAge, time.Now(), cfg.Block.Countries)
	switch {
	case errors.Is(err, diskcache.ErrAbsent):
		logger.Info("disk cache: none present; cold start", "path", cfg.Cache.Path)
	case errors.Is(err, diskcache.ErrStale):
		logger.Warn("disk cache: stale; skipping", "path", cfg.Cache.Path, "max_age", cfg.Cache.MaxAge)
	case err != nil:
		// Corrupt or unreadable cache: remove it so it does not
		// re-trip the next start (OQ-CACHE-2 — Architect lean).
		logger.Warn("disk cache: unreadable; skipping and removing", "path", cfg.Cache.Path, "error", err)
		if rmErr := os.Remove(cfg.Cache.Path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			logger.Warn("disk cache: could not remove corrupt file", "path", cfg.Cache.Path, "error", rmErr)
		}
	default:
		src.Swap(trie)
		logger.Info("disk cache: loaded blocklist from disk", "path", cfg.Cache.Path, "prefixes", trie.Len())
	}
}

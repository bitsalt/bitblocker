// Command bitblocker is the BitBlocker daemon entry point.
//
// It wires configuration, logging, the blocklist cache, the data
// fetcher, and the forwardAuth HTTP server. See docs/BitBlocker.md for
// the product overview and docs/bitblocker-spec.md for the component
// specification.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/diskcache"
	"github.com/bitsalt/bitblocker/internal/fetcher"
	"github.com/bitsalt/bitblocker/internal/logging"
	"github.com/bitsalt/bitblocker/internal/scheduler"
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
	// pointer; the scheduler Swaps a fresh trie in on every successful
	// refresh. The disk cache below gives the daemon a head start ahead
	// of the first network fetch.
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

	fetch, err := newFetcher(cfg, src, logger)
	if err != nil {
		return fmt.Errorf("fetcher: %w", err)
	}

	sched, err := buildScheduler(cfg, fetch, src, logger)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runComponents(ctx, srv, sched, logger); err != nil {
		return err
	}

	logger.Info("bitblocker: stopped")
	return nil
}

// buildScheduler wires the periodic-refresh scheduler over the fetcher.
// The Ready predicate reports whether a usable (cached) blocklist is
// already active, so a failed cold-start refresh defers to the cron
// cadence instead of burning the retry budget.
func buildScheduler(cfg *config.Config, fetch *fetcher.Fetcher, src *blocklist.Source, logger *slog.Logger) (*scheduler.Scheduler, error) {
	return scheduler.New(scheduler.Options{
		Schedule: cfg.Refresh.Schedule,
		Timeout:  cfg.Refresh.Timeout,
		Refresh: func(ctx context.Context) error {
			_, rerr := fetch.Refresh(ctx)
			return rerr
		},
		Ready:  func() bool { t := src.Current(); return t != nil && t.Len() > 0 },
		Logger: logger,
	})
}

// runComponents runs the scheduler and the HTTP server concurrently
// until ctx is cancelled or either exits, then drains the other. runCtx
// lets the server's exit tear down the scheduler (and vice versa) even
// when no OS signal arrived — e.g. a bind failure.
func runComponents(ctx context.Context, srv *server.Server, sched *scheduler.Scheduler, logger *slog.Logger) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if serr := sched.Run(runCtx); serr != nil {
			logger.Error("scheduler: stopped with error", "error", serr)
		}
	}()

	runErr := srv.Run(runCtx)
	cancel()
	wg.Wait()

	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", runErr)
	}
	return nil
}

// newFetcher constructs the DB-IP fetcher with an HTTPS client pinned to
// a TLS 1.2 floor (Go addendum §8). The per-fetch timeout is applied
// both here and via the scheduler's context deadline.
func newFetcher(cfg *config.Config, src *blocklist.Source, logger *slog.Logger) (*fetcher.Fetcher, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return fetcher.New(fetcher.Options{
		HTTPClient: &http.Client{Timeout: cfg.Refresh.Timeout, Transport: transport},
		CachePath:  cfg.Cache.Path,
		Countries:  cfg.Block.Countries,
		Source:     src,
		Logger:     logger,
	})
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
		// Stale cache: remove it so it does not re-trip the next start's
		// load attempt + WARN (OQ-CACHE-2 — ratified 2026-07-19).
		logger.Warn("disk cache: stale; skipping and removing", "path", cfg.Cache.Path, "max_age", cfg.Cache.MaxAge)
		removeUnusableCache(logger, cfg.Cache.Path)
	case err != nil:
		// Corrupt or unreadable cache: remove it for the same reason
		// (OQ-CACHE-2).
		logger.Warn("disk cache: unreadable; skipping and removing", "path", cfg.Cache.Path, "error", err)
		removeUnusableCache(logger, cfg.Cache.Path)
	default:
		src.Swap(trie)
		logger.Info("disk cache: loaded blocklist from disk", "path", cfg.Cache.Path, "prefixes", trie.Len())
	}
}

// removeUnusableCache deletes a cache file that failed to load — stale
// or corrupt — so it does not re-trip the load attempt (and its WARN) on
// the next start (OQ-CACHE-2). Removal is best-effort and non-fatal:
// like a failed cache write, a removal failure is logged at WARN and the
// daemon continues its fail-closed cold start. A file already gone
// (os.ErrNotExist) is treated as success.
func removeUnusableCache(logger *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("disk cache: could not remove unusable file", "path", path, "error", err)
	}
}

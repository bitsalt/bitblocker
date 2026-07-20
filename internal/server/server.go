package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/logging"
)

// Lookup is the read-side contract a populated blocklist must satisfy
// for the HTTP server to make a /check decision. The server depends on
// this interface — defined here at the consumer — rather than on the
// concrete *blocklist.Trie, so the swap mechanism that lands in a
// separate slice can return any type satisfying these methods.
type Lookup interface {
	// Contains reports whether ip is blocked by the active ruleset.
	Contains(ip netip.Addr) bool
	// Len returns the number of distinct prefixes in the active
	// ruleset. Zero means the daemon is not yet ready to serve.
	Len() int
}

// LookupSource yields the active Lookup. Implementations are expected
// to be cheap on the hot path: in production today this is a closure
// over a *blocklist.Trie; once the atomic-swap slice lands it will
// dereference an atomic pointer.
type LookupSource func() Lookup

// Server is the forwardAuth HTTP surface. It owns the routing table
// (/check, /healthz), the IP-extraction policy, and the fail-closed
// translation between "I cannot decide" and "deny."
type Server struct {
	addr        string
	lookup      LookupSource
	logger      *slog.Logger
	blockStatus int
	logBlocked  bool
	logAllowed  bool
	readiness   *readiness
}

// Options configures a Server. All fields are required — the constructor
// applies no defaults of its own; the daemon's Config has already
// supplied them.
type Options struct {
	// Addr is the host:port the server listens on.
	Addr string
	// Lookup yields the current blocklist. Called once per /check.
	Lookup LookupSource
	// Logger is propagated into the per-request context. The daemon
	// constructs it once from internal/logging.New.
	Logger *slog.Logger
	// BlockStatus is the HTTP status returned for blocked clients
	// and for fail-closed denials. Sourced from
	// behavior.response_code.
	BlockStatus int
	// LogBlocked controls whether /check emits an INFO line on a
	// blocked decision. Fail-closed denials are always WARN
	// regardless of this flag.
	LogBlocked bool
	// LogAllowed controls whether /check emits a DEBUG line on an
	// allowed decision.
	LogAllowed bool
	// StartupMode selects the /check behavior while the blocklist is
	// unusable: fail-closed denies, fail-open allows. It has no effect
	// once a usable blocklist is loaded, and never affects /healthz or
	// the unparseable-client-IP path. See ADR 0004.
	StartupMode config.StartupMode
}

// New constructs a Server from opts. It validates required fields but
// does not start listening — call Run for that.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("server: Addr is required")
	}
	if opts.Lookup == nil {
		return nil, errors.New("server: Lookup is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("server: Logger is required")
	}
	if opts.BlockStatus < 400 || opts.BlockStatus > 599 {
		return nil, fmt.Errorf("server: BlockStatus must be 4xx/5xx, got %d", opts.BlockStatus)
	}
	// No default is applied: an unset StartupMode is a wiring bug, and
	// silently picking one would decide the daemon's security posture on
	// the caller's behalf.
	if opts.StartupMode != config.StartupFailClosed && opts.StartupMode != config.StartupFailOpen {
		return nil, fmt.Errorf("server: StartupMode must be %q or %q, got %q",
			config.StartupFailClosed, config.StartupFailOpen, opts.StartupMode)
	}

	return &Server{
		addr:        opts.Addr,
		lookup:      opts.Lookup,
		logger:      opts.Logger,
		blockStatus: opts.BlockStatus,
		logBlocked:  opts.LogBlocked,
		logAllowed:  opts.LogAllowed,
		readiness:   newReadiness(opts.StartupMode, opts.Logger, time.Now),
	}, nil
}

// AddrFromConfig builds a host:port string from a validated
// ListenConfig. Exposed so cmd/bitblocker does not have to repeat the
// formatting rule.
func AddrFromConfig(c config.ListenConfig) string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Handler returns an http.Handler wiring /check and /healthz. Exposed
// for tests; Run wraps the same handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/check", s.handleCheck)
	mux.HandleFunc("/healthz", s.handleHealthz)
	return s.withRequestLogger(mux)
}

// Run binds and serves until ctx is cancelled. It returns the first
// non-shutdown error from the underlying http.Server.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Observe readiness before serving so a cold start reports its
	// unusable state immediately, rather than waiting for the first
	// request or the first heartbeat tick. An inert daemon receiving no
	// traffic at all must still say so.
	s.readiness.observe(s.lookup())

	hbCtx, hbCancel := context.WithCancel(ctx)
	ticker := time.NewTicker(heartbeatInterval)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		s.heartbeatLoop(hbCtx, ticker.C)
	}()
	defer func() {
		hbCancel()
		ticker.Stop()
		hbWG.Wait()
	}()

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server: listening", "addr", s.addr)
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// heartbeatLoop re-reports a persistent unusable-blocklist state on
// wall-clock cadence until ctx is cancelled. It is driven by tick rather
// than by an internal ticker so tests can step it deterministically.
//
// Cadence, not request activity, is the point: a daemon serving zero
// traffic while holding no blocklist is precisely the case this signal
// exists to surface.
func (s *Server) heartbeatLoop(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.readiness.heartbeat(s.lookup())
		}
	}
}

// withRequestLogger attaches the server's logger to the per-request
// context so handlers can pick it up via logging.FromContext. Future
// correlation-id work hangs off this seam.
func (s *Server) withRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithContext(r.Context(), s.logger)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// handleCheck implements the forwardAuth contract. It only accepts
// GET; any other method is 405. Blocked or unparseable requests get
// the configured block status with an empty body. Allowed requests
// get 200 with an empty body.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	logger := logging.FromContext(r.Context())

	// One Lookup read per request: a concurrent Swap between two reads
	// would let a single request evaluate two different tries.
	lookup := s.lookup()
	if !s.readiness.observe(lookup) {
		// The unusable state is reported by the transition and heartbeat
		// signals, never per request — at real request rates behind
		// Traefik a per-request line is a log flood during exactly the
		// incident an operator needs to read the log through.
		if s.readiness.failOpen() {
			s.readiness.countFailOpenAllow()
			// Client-IP extraction is skipped: the decision does not
			// depend on the address, and extracting it would emit a
			// misleading unparseable-IP signal for a request that is
			// being allowed regardless.
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(s.blockStatus)
		return
	}

	// An unparseable client IP is fail-closed under BOTH startup modes,
	// permanently. startup_mode governs data availability, not input
	// validity: X-Real-IP / X-Forwarded-For is attacker-influenced, so
	// honoring fail-open here would hand any client a total blocklist
	// bypass via a malformed header — even against a fully populated
	// daemon. See ADR 0004 §A.1 and interface spec §3.3. Do not
	// generalize the fail-open branch above across this one.
	addr, ok := extractClientIP(r)
	if !ok {
		logger.Warn("check: fail-closed (unparseable client IP)",
			"x_real_ip_present", r.Header.Get(headerXRealIP) != "",
			"x_forwarded_for_present", r.Header.Get(headerXForwardedFor) != "",
		)
		w.WriteHeader(s.blockStatus)
		return
	}

	if lookup.Contains(addr) {
		if s.logBlocked {
			logger.Info("check: blocked",
				"client_ip_redacted", logging.Redact(addr.String()),
			)
		}
		w.WriteHeader(s.blockStatus)
		return
	}

	if s.logAllowed {
		logger.Debug("check: allowed",
			"client_ip_redacted", logging.Redact(addr.String()),
		)
	}
	w.WriteHeader(http.StatusOK)
}

// healthzResponse is the JSON envelope /healthz returns. The shape is
// stable and documented in docs/bitblocker-spec.md.
//
// The Status value domain ("ok" | "empty") is a published contract and
// does not change; the discriminator fields below were added additively
// so consumers parsing only Status keep working (ADR 0004 §C, coding
// standards §14 — extend, do not redefine). Future changes here must
// stay additive.
type healthzResponse struct {
	Status    string `json:"status"`
	Serving   string `json:"serving"`
	EverReady bool   `json:"ever_ready"`
	// Prefixes is present only when the blocklist is usable.
	Prefixes *int `json:"prefixes,omitempty"`
	// EmptyForSeconds is present only when it is not.
	EmptyForSeconds *int64 `json:"empty_for_seconds,omitempty"`
}

// handleHealthz implements the readiness probe. While the blocklist is
// empty (cold start, fetch never succeeded) the daemon is not yet
// ready to make authorization decisions and returns 503.
//
// This is deliberately independent of startup_mode: readiness means
// "ready to make authorization decisions," not "the HTTP server is
// answering." Returning 200 under fail-open would let the feature most
// likely to hide a non-functioning daemon delete the one signal that
// reveals it — orchestrators and monitoring keyed on /healthz would
// report a healthy daemon that is blocking nothing. Read ADR 0004 §C
// before changing this.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}

	lookup := s.lookup()
	w.Header().Set("Content-Type", "application/json")

	if !s.readiness.observe(lookup) {
		emptyFor := int64(s.readiness.windowSince() / time.Second)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(healthzResponse{
			Status:          "empty",
			Serving:         s.readiness.serving(false),
			EverReady:       s.readiness.everReady.Load(),
			EmptyForSeconds: &emptyFor,
		})
		return
	}

	prefixes := lookup.Len()
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthzResponse{
		Status:    "ok",
		Serving:   servingEnforcing,
		EverReady: true,
		Prefixes:  &prefixes,
	})
}

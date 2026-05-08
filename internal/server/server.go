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

	return &Server{
		addr:        opts.Addr,
		lookup:      opts.Lookup,
		logger:      opts.Logger,
		blockStatus: opts.BlockStatus,
		logBlocked:  opts.LogBlocked,
		logAllowed:  opts.LogAllowed,
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

	lookup := s.lookup()
	if lookup == nil || lookup.Len() == 0 {
		logger.Warn("check: fail-closed (blocklist not ready)",
			"path", r.URL.Path,
			"remote_addr_redacted", logging.Redact(r.RemoteAddr),
		)
		w.WriteHeader(s.blockStatus)
		return
	}

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
type healthzResponse struct {
	Status string `json:"status"`
}

// handleHealthz implements the readiness probe. While the blocklist is
// empty (cold start, fetch never succeeded) the daemon is not yet
// ready to make authorization decisions and returns 503.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}

	lookup := s.lookup()
	w.Header().Set("Content-Type", "application/json")

	if lookup == nil || lookup.Len() == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(healthzResponse{Status: "empty"})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthzResponse{Status: "ok"})
}

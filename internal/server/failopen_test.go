package server_test

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/server"
)

// failOpen is a newServer option applying fail-open startup mode.
func failOpen(o *server.Options) { o.StartupMode = config.StartupFailOpen }

// TestCheck_DecisionTable exercises every row of the /check decision
// table under both startup modes. Only the unusable-blocklist rows may
// differ between modes; every other row must be identical.
func TestCheck_DecisionTable(t *testing.T) {
	populated := func() server.Lookup {
		return &stubLookup{contains: map[string]bool{"203.0.113.7": true}, length: 5}
	}

	cases := []struct {
		name           string
		lookup         server.Lookup
		method         string
		headers        map[string]string
		wantFailClosed int
		wantFailOpen   int
	}{
		{
			name:           "non-GET is 405 under both modes",
			lookup:         populated(),
			method:         http.MethodPost,
			headers:        map[string]string{"X-Real-IP": "198.51.100.1"},
			wantFailClosed: http.StatusMethodNotAllowed,
			wantFailOpen:   http.StatusMethodNotAllowed,
		},
		{
			name:           "nil lookup diverges by mode",
			lookup:         nil,
			method:         http.MethodGet,
			headers:        map[string]string{"X-Real-IP": "198.51.100.1"},
			wantFailClosed: http.StatusForbidden,
			wantFailOpen:   http.StatusOK,
		},
		{
			name:           "empty lookup diverges by mode",
			lookup:         &stubLookup{length: 0},
			method:         http.MethodGet,
			headers:        map[string]string{"X-Real-IP": "198.51.100.1"},
			wantFailClosed: http.StatusForbidden,
			wantFailOpen:   http.StatusOK,
		},
		{
			name:           "populated plus unparseable IP is blocked under both modes",
			lookup:         populated(),
			method:         http.MethodGet,
			headers:        map[string]string{"X-Real-IP": "not-an-ip"},
			wantFailClosed: http.StatusForbidden,
			wantFailOpen:   http.StatusForbidden,
		},
		{
			name:           "populated plus listed IP is blocked under both modes",
			lookup:         populated(),
			method:         http.MethodGet,
			headers:        map[string]string{"X-Real-IP": "203.0.113.7"},
			wantFailClosed: http.StatusForbidden,
			wantFailOpen:   http.StatusForbidden,
		},
		{
			name:           "populated plus unlisted IP is allowed under both modes",
			lookup:         populated(),
			method:         http.MethodGet,
			headers:        map[string]string{"X-Real-IP": "198.51.100.1"},
			wantFailClosed: http.StatusOK,
			wantFailOpen:   http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("fail-closed", func(t *testing.T) {
				srv, _ := newServer(t, tc.lookup)
				w := do(t, srv.Handler(), tc.method, "/check", tc.headers)
				require.Equal(t, tc.wantFailClosed, w.Code)
			})
			t.Run("fail-open", func(t *testing.T) {
				srv, _ := newServer(t, tc.lookup, failOpen)
				w := do(t, srv.Handler(), tc.method, "/check", tc.headers)
				require.Equal(t, tc.wantFailOpen, w.Code)
			})
		})
	}
}

// TestCheck_FailOpenDoesNotApplyToUnparseableClientIP is the regression
// guard for the security boundary this feature exists alongside.
//
// This behavior is INTENTIONAL and must not be "fixed": startup_mode
// governs data availability, not input validity. X-Real-IP and
// X-Forwarded-For are attacker-influenced, so honoring fail-open here
// would let any client bypass a fully populated blocklist by sending a
// malformed header. A populated blocklist plus fail-open plus a
// malformed header must still block. See ADR 0004 §A.1, interface spec
// §3.3.
func TestCheck_FailOpenDoesNotApplyToUnparseableClientIP(t *testing.T) {
	stub := &stubLookup{contains: map[string]bool{}, length: 5}

	malformed := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers at all", nil},
		{"junk x-real-ip and junk xff", map[string]string{
			"X-Real-IP":       "not-an-ip",
			"X-Forwarded-For": "also-junk",
		}},
		{"empty xff entries only", map[string]string{"X-Forwarded-For": ", ,"}},
	}

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t, stub, failOpen)
			w := do(t, srv.Handler(), http.MethodGet, "/check", tc.headers)
			require.Equal(t, http.StatusForbidden, w.Code,
				"fail-open must never allow a request whose client IP cannot be parsed")
		})
	}
}

// TestHealthz_503UnderFailOpen guards the second boundary: /healthz must
// not be taught to report a fail-open daemon as healthy. Readiness means
// "ready to make authorization decisions," not "the HTTP server is
// answering," so fail-open must not be able to mask itself from the one
// probe orchestrators key on. See ADR 0004 §C.
func TestHealthz_503UnderFailOpen(t *testing.T) {
	for _, lookup := range []server.Lookup{nil, &stubLookup{length: 0}} {
		srv, _ := newServer(t, lookup, failOpen)

		w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)
		require.Equal(t, http.StatusServiceUnavailable, w.Code,
			"an unusable blocklist is 503 regardless of startup_mode")

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "empty", body["status"], "the status value domain must not change")
		require.Equal(t, "allow-all", body["serving"])
		require.Equal(t, false, body["ever_ready"])
		require.Contains(t, body, "empty_for_seconds")
		require.NotContains(t, body, "prefixes")
	}
}

func TestHealthz_DiscriminatorFields(t *testing.T) {
	t.Run("unusable under fail-closed reports deny-all", func(t *testing.T) {
		srv, _ := newServer(t, &stubLookup{length: 0})
		w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "empty", body["status"])
		require.Equal(t, "deny-all", body["serving"])
	})

	t.Run("usable reports enforcing with a prefix count", func(t *testing.T) {
		srv, _ := newServer(t, &stubLookup{length: 184213})
		w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "ok", body["status"])
		require.Equal(t, "enforcing", body["serving"])
		require.Equal(t, true, body["ever_ready"])
		require.Equal(t, float64(184213), body["prefixes"])
		require.NotContains(t, body, "empty_for_seconds")
	})
}

// TestCheck_NoPerRequestLoggingWhileUnusable is the negative guard for
// the removed per-request WARN. The unusable state is reported once per
// transition plus a heartbeat; request volume must not drive log volume.
func TestCheck_NoPerRequestLoggingWhileUnusable(t *testing.T) {
	for _, mode := range []func(*server.Options){nil, failOpen} {
		opts := []func(*server.Options){}
		if mode != nil {
			opts = append(opts, mode)
		}
		srv, logs := newServer(t, &stubLookup{length: 0}, opts...)

		const requests = 50
		for range requests {
			do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
				"X-Real-IP": "198.51.100.1",
			})
		}

		lines := parseLogLines(t, logs)
		require.Len(t, lines, 1,
			"50 requests under an unusable blocklist must produce exactly one line: the transition")
		require.Equal(t, "ERROR", lines[0]["level"])
	}
}

// TestCheck_FailOpenColdStartThroughRealLookupSource exercises the
// production nil-returning path rather than a stub. The real
// LookupSource closure returns an UNTYPED nil before any swap; a
// fail-open branch that reached Len() on it would panic on a non-nil
// interface holding a nil pointer (ADR 0001, interface spec
// blocklist-source.md §2.5).
func TestCheck_FailOpenColdStartThroughRealLookupSource(t *testing.T) {
	src := blocklist.NewSource()
	srv, _ := newServer(t, nil, failOpen, func(o *server.Options) {
		o.Lookup = func() server.Lookup {
			trie := src.Current()
			if trie == nil {
				return nil
			}
			return trie
		}
	})

	require.NotPanics(t, func() {
		w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
			"X-Real-IP": "198.51.100.1",
		})
		require.Equal(t, http.StatusOK, w.Code)
	})
}

// TestReadiness_TransitionsEmitOnceUnderConcurrency hammers /check
// across a swap. The transition signals are once-per-transition, so
// concurrent observers must not each emit their own copy.
func TestReadiness_TransitionsEmitOnceUnderConcurrency(t *testing.T) {
	var mu sync.Mutex
	trie := blocklist.New()
	current := server.Lookup(nil)

	srv, logs := newServer(t, nil, failOpen, func(o *server.Options) {
		o.Lookup = func() server.Lookup {
			mu.Lock()
			defer mu.Unlock()
			return current
		}
	})

	// Force the entering transition before the racers start, so the test
	// asserts once-per-transition rather than racing the publish against
	// the first request.
	do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{"X-Real-IP": "198.51.100.1"})

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 25 {
				do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
					"X-Real-IP": "198.51.100.1",
				})
			}
		}()
	}
	close(start)

	// Publish a usable blocklist mid-flight so goroutines observe the
	// transition concurrently.
	trie.Insert(netip.MustParsePrefix("203.0.113.0/24"))
	mu.Lock()
	current = trie
	mu.Unlock()
	wg.Wait()

	// Drive one final observation so the recovery signal is guaranteed
	// to have been reached even if every goroutine finished early.
	do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{"X-Real-IP": "198.51.100.1"})

	body := logs.String()
	require.Equal(t, 1, strings.Count(body, "blocklist unusable; ALLOWING ALL REQUESTS"),
		"entering the unusable state must be reported exactly once")
	require.Equal(t, 1, strings.Count(body, "normal enforcement resumed"),
		"recovery must be reported exactly once")
}

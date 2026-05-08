package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/server"
)

// stubLookup is a minimal in-test Lookup implementation. The real
// production lookup is *blocklist.Trie (also exercised in the
// integration-shaped tests below); the stub gives finer control over
// Len() in healthz scenarios.
type stubLookup struct {
	contains map[string]bool
	length   int
}

func (s *stubLookup) Contains(ip netip.Addr) bool {
	return s.contains[ip.Unmap().String()]
}

func (s *stubLookup) Len() int { return s.length }

// newServer builds a *Server wired to the given Lookup and returns it
// alongside the buffer the JSON logger writes into so tests can assert
// log emissions.
func newServer(t *testing.T, lookup server.Lookup, opts ...func(*server.Options)) (*server.Server, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	o := server.Options{
		Addr:        "127.0.0.1:0",
		Lookup:      func() server.Lookup { return lookup },
		Logger:      logger,
		BlockStatus: http.StatusForbidden,
		LogBlocked:  true,
		LogAllowed:  false,
	}
	for _, fn := range opts {
		fn(&o)
	}
	srv, err := server.New(o)
	require.NoError(t, err)
	return srv, buf
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, http.NoBody)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		out = append(out, rec)
	}
	return out
}

func TestNew_RejectsMissingFields(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name string
		opts server.Options
	}{
		{"missing addr", server.Options{Lookup: func() server.Lookup { return nil }, Logger: logger, BlockStatus: 403}},
		{"missing lookup", server.Options{Addr: ":0", Logger: logger, BlockStatus: 403}},
		{"missing logger", server.Options{Addr: ":0", Lookup: func() server.Lookup { return nil }, BlockStatus: 403}},
		{"out-of-range status", server.Options{Addr: ":0", Lookup: func() server.Lookup { return nil }, Logger: logger, BlockStatus: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := server.New(tc.opts)
			require.Error(t, err)
		})
	}
}

func TestCheck_AllowsUnblockedIP(t *testing.T) {
	stub := &stubLookup{contains: map[string]bool{}, length: 5}
	srv, _ := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Body.String(), "/check body must be empty")
}

func TestCheck_BlocksListedIP(t *testing.T) {
	stub := &stubLookup{
		contains: map[string]bool{"203.0.113.7": true},
		length:   1,
	}
	srv, logs := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, w.Body.String())

	// Block decision must log at INFO with a redacted IP — never the raw value.
	lines := parseLogLines(t, logs)
	require.NotEmpty(t, lines)
	last := lines[len(lines)-1]
	require.Equal(t, "INFO", last["level"])
	require.Equal(t, "check: blocked", last["msg"])
	require.NotContains(t, logs.String(), "203.0.113.7", "raw client IP must not appear in logs")
}

func TestCheck_HonorsConfiguredBlockStatus(t *testing.T) {
	stub := &stubLookup{contains: map[string]bool{"203.0.113.7": true}, length: 1}
	srv, _ := newServer(t, stub, func(o *server.Options) {
		o.BlockStatus = http.StatusTeapot
	})

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	require.Equal(t, http.StatusTeapot, w.Code)
}

func TestCheck_FailClosedOnUnparseableIP(t *testing.T) {
	stub := &stubLookup{contains: map[string]bool{}, length: 5}
	srv, logs := newServer(t, stub)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers", nil},
		{"junk x-real-ip and junk xff", map[string]string{
			"X-Real-IP":       "not-an-ip",
			"X-Forwarded-For": "also-junk",
		}},
		{"empty xff entries only", map[string]string{
			"X-Forwarded-For": ", ,",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			w := do(t, srv.Handler(), http.MethodGet, "/check", tc.headers)
			require.Equal(t, http.StatusForbidden, w.Code, "fail-closed must use BlockStatus")
			require.Empty(t, w.Body.String())

			lines := parseLogLines(t, logs)
			require.NotEmpty(t, lines)
			last := lines[len(lines)-1]
			require.Equal(t, "WARN", last["level"], "unparseable IP must log at WARN")
			require.Equal(t, "check: fail-closed (unparseable client IP)", last["msg"])
		})
	}
}

func TestCheck_FailClosedWhenBlocklistEmpty(t *testing.T) {
	stub := &stubLookup{length: 0}
	srv, logs := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, w.Body.String())

	lines := parseLogLines(t, logs)
	require.NotEmpty(t, lines)
	require.Equal(t, "WARN", lines[len(lines)-1]["level"])
	require.Equal(t, "check: fail-closed (blocklist not ready)", lines[len(lines)-1]["msg"])
}

func TestCheck_FailClosedWhenLookupNil(t *testing.T) {
	// LookupSource may legitimately return nil during cold start
	// before any swap has published a trie. The handler must treat
	// that the same as an empty blocklist.
	srv, _ := newServer(t, nil, func(o *server.Options) {
		o.Lookup = func() server.Lookup { return nil }
	})

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestCheck_RejectsNonGet(t *testing.T) {
	stub := &stubLookup{length: 1}
	srv, _ := newServer(t, stub)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := do(t, srv.Handler(), method, "/check", map[string]string{"X-Real-IP": "203.0.113.7"})
			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
			require.Equal(t, http.MethodGet, w.Header().Get("Allow"))
		})
	}
}

func TestCheck_IntegratesWithRealTrie(t *testing.T) {
	// Sanity-check that the production *blocklist.Trie satisfies the
	// server.Lookup interface and that lookups work end-to-end.
	tr := blocklist.New()
	tr.Insert(netip.MustParsePrefix("203.0.113.0/24"))
	tr.Insert(netip.MustParsePrefix("2001:db8::/32"))

	srv, _ := newServer(t, tr)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"ipv4 in blocked /24", "203.0.113.99", http.StatusForbidden},
		{"ipv4 outside blocked /24", "198.51.100.1", http.StatusOK},
		{"ipv6 in blocked /32", "2001:db8::beef", http.StatusForbidden},
		{"ipv6 outside blocked /32", "2001:db9::1", http.StatusOK},
		{"ipv4-mapped ipv6 in blocked /24", "::ffff:203.0.113.99", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
				"X-Real-IP": tc.header,
			})
			require.Equal(t, tc.want, w.Code)
		})
	}
}

func TestCheck_RightmostXFFFallback(t *testing.T) {
	stub := &stubLookup{
		contains: map[string]bool{"203.0.113.7": true},
		length:   1,
	}
	srv, _ := newServer(t, stub)

	// Leftmost (1.1.1.1) is attacker-controllable; rightmost
	// (203.0.113.7) reflects the trusted hop and must drive the
	// decision.
	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Forwarded-For": "1.1.1.1, 198.51.100.2, 203.0.113.7",
	})
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestCheck_LogAllowedFlag(t *testing.T) {
	stub := &stubLookup{length: 1}
	srv, logs := newServer(t, stub, func(o *server.Options) {
		o.LogAllowed = true
	})

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	require.Equal(t, http.StatusOK, w.Code)

	lines := parseLogLines(t, logs)
	require.NotEmpty(t, lines)
	require.Equal(t, "DEBUG", lines[len(lines)-1]["level"])
	require.Equal(t, "check: allowed", lines[len(lines)-1]["msg"])
}

func TestCheck_LogBlockedDisabled(t *testing.T) {
	stub := &stubLookup{
		contains: map[string]bool{"203.0.113.7": true},
		length:   1,
	}
	srv, logs := newServer(t, stub, func(o *server.Options) {
		o.LogBlocked = false
	})

	w := do(t, srv.Handler(), http.MethodGet, "/check", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	require.Equal(t, http.StatusForbidden, w.Code)

	// No log line should be emitted for a normal blocked decision
	// when LogBlocked is off.
	require.Empty(t, strings.TrimSpace(logs.String()))
}

func TestHealthz_503WhileEmpty(t *testing.T) {
	stub := &stubLookup{length: 0}
	srv, _ := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var body healthzBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "empty", body.Status)
}

func TestHealthz_503WhenLookupNil(t *testing.T) {
	srv, _ := newServer(t, nil, func(o *server.Options) {
		o.Lookup = func() server.Lookup { return nil }
	})

	w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body healthzBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "empty", body.Status)
}

func TestHealthz_200WhenPopulated(t *testing.T) {
	stub := &stubLookup{length: 1}
	srv, _ := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var body healthzBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "ok", body.Status)
}

func TestHealthz_RejectsNonGet(t *testing.T) {
	stub := &stubLookup{length: 1}
	srv, _ := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodPost, "/healthz", nil)
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestUnknownPathReturns404(t *testing.T) {
	stub := &stubLookup{length: 1}
	srv, _ := newServer(t, stub)

	w := do(t, srv.Handler(), http.MethodGet, "/nope", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestAddrFromConfig(t *testing.T) {
	require.Equal(t, "127.0.0.1:8080", server.AddrFromConfig(configListen("127.0.0.1", 8080)))
	require.Equal(t, "[::1]:8080", server.AddrFromConfig(configListen("::1", 8080)))
}

// healthzBody mirrors the server's JSON envelope for /healthz.
type healthzBody struct {
	Status string `json:"status"`
}

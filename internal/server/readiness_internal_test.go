package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/config"
)

// These tests live in package server rather than server_test because
// they drive the heartbeat ticker and the readiness clock directly.
// Both are deliberately unexported: the heartbeat interval is a constant
// rather than a config knob (ADR 0004 §E), so it must not be reachable
// through the public Options surface just to make it testable.

// fakeLookup is a Lookup whose length the test controls.
type fakeLookup struct{ length int }

func (f *fakeLookup) Contains(netip.Addr) bool { return false }
func (f *fakeLookup) Len() int                 { return f.length }

// testServer builds a Server with a controllable clock and Lookup.
func testServer(t *testing.T, mode config.StartupMode, lookup func() Lookup) (*Server, *bytes.Buffer, *fakeClock) {
	t.Helper()
	buf := &bytes.Buffer{}
	clock := &fakeClock{at: time.Unix(1_700_000_000, 0)}

	srv, err := New(Options{
		Addr:        "127.0.0.1:0",
		Lookup:      lookup,
		Logger:      slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		BlockStatus: http.StatusForbidden,
		StartupMode: mode,
	})
	require.NoError(t, err)
	srv.readiness.now = clock.Now
	return srv, buf, clock
}

// fakeClock is a manually advanced time source.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
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

func recordsWithMsg(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// TestHeartbeat_FiresWithNoTrafficAtAll is the inert-daemon case: a
// daemon that never acquires a blocklist and receives zero requests must
// still report itself, on wall-clock cadence, forever.
func TestHeartbeat_FiresWithNoTrafficAtAll(t *testing.T) {
	srv, logs, clock := testServer(t, config.StartupFailOpen, func() Lookup { return nil })

	// The cold-start entry signal, as Run emits it before serving.
	srv.readiness.observe(srv.lookup())

	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.heartbeatLoop(ctx, tick)
	}()

	for i := 1; i <= 3; i++ {
		clock.advance(heartbeatInterval)
		tick <- clock.Now()
	}
	// A fourth send is only accepted once the third has been consumed,
	// so this synchronizes on the third heartbeat having been emitted.
	clock.advance(heartbeatInterval)
	tick <- clock.Now()
	cancel()
	<-done

	beats := recordsWithMsg(logRecords(t, logs), "check: blocklist still unusable")
	require.Len(t, beats, 4, "heartbeat must fire per tick with no request traffic")

	last := beats[len(beats)-1]
	require.Equal(t, "ERROR", last["level"])
	require.Equal(t, false, last["ever_ready"], "a daemon that never loaded a blocklist reports ever_ready:false")
	require.Equal(t, "allow-all", last["serving"])
	require.Equal(t, "fail-open", last["startup_mode"])
	require.Equal(t, "no successful fetch since start", last["likely_cause"])
	require.Equal(t, "4m0s", last["unusable_for"])
}

// TestHeartbeat_ReadyThenEmptyReportsConfigCause covers the second
// unusable sub-state: a blocklist that loaded successfully but matched
// no configured countries. Its fix is a config correction, not a
// network fix, so it must not be reported as a failed fetch.
func TestHeartbeat_ReadyThenEmptyReportsConfigCause(t *testing.T) {
	lookup := &fakeLookup{length: 5}
	srv, logs, clock := testServer(t, config.StartupFailClosed, func() Lookup { return lookup })

	srv.readiness.observe(srv.lookup())
	require.True(t, srv.readiness.everReady.Load())

	lookup.length = 0
	srv.readiness.observe(srv.lookup())

	clock.advance(heartbeatInterval)
	srv.readiness.heartbeat(srv.lookup())

	beats := recordsWithMsg(logRecords(t, logs), "check: blocklist still unusable")
	require.Len(t, beats, 1)
	require.Equal(t, true, beats[0]["ever_ready"])
	require.Equal(t, "deny-all", beats[0]["serving"])
	require.Equal(t,
		"blocklist loaded but matched no configured countries — check block.countries",
		beats[0]["likely_cause"])
	require.NotContains(t, beats[0], "failopen_allowed_total",
		"fail-closed must not report fail-open counters")
	require.NotContains(t, beats[0], "failopen_allowed_since_last")
}

// TestHeartbeat_FailOpenCounters checks the counter contract: the total
// accumulates across heartbeats, the since-last figure resets on each.
func TestHeartbeat_FailOpenCounters(t *testing.T) {
	srv, logs, clock := testServer(t, config.StartupFailOpen, func() Lookup { return nil })
	handler := srv.Handler()

	allow := func(n int) {
		for range n {
			r := httptest.NewRequest(http.MethodGet, "/check", http.NoBody)
			r.Header.Set("X-Real-IP", "198.51.100.1")
			handler.ServeHTTP(httptest.NewRecorder(), r)
		}
	}

	allow(3)
	clock.advance(heartbeatInterval)
	srv.readiness.heartbeat(srv.lookup())

	allow(2)
	clock.advance(heartbeatInterval)
	srv.readiness.heartbeat(srv.lookup())

	beats := recordsWithMsg(logRecords(t, logs), "check: blocklist still unusable")
	require.Len(t, beats, 2)
	require.Equal(t, float64(3), beats[0]["failopen_allowed_total"])
	require.Equal(t, float64(3), beats[0]["failopen_allowed_since_last"])
	require.Equal(t, float64(5), beats[1]["failopen_allowed_total"], "total must accumulate")
	require.Equal(t, float64(2), beats[1]["failopen_allowed_since_last"], "since-last must reset per heartbeat")
}

// TestReadiness_EverReadyLatches confirms ever_ready is a latch, not a
// mirror of the current state: once the daemon has functioned, a later
// return to unusable must not make it look like it never did.
func TestReadiness_EverReadyLatches(t *testing.T) {
	lookup := &fakeLookup{length: 0}
	srv, _, _ := testServer(t, config.StartupFailClosed, func() Lookup { return lookup })

	srv.readiness.observe(srv.lookup())
	require.False(t, srv.readiness.everReady.Load())

	lookup.length = 3
	srv.readiness.observe(srv.lookup())
	require.True(t, srv.readiness.everReady.Load())

	lookup.length = 0
	srv.readiness.observe(srv.lookup())
	require.True(t, srv.readiness.everReady.Load(), "ever_ready must never reset")
}

// TestReadiness_RecoverySignal covers the window-closing INFO: how long
// the window ran and what it let through.
func TestReadiness_RecoverySignal(t *testing.T) {
	lookup := &fakeLookup{length: 0}
	srv, logs, clock := testServer(t, config.StartupFailOpen, func() Lookup { return lookup })
	handler := srv.Handler()

	srv.readiness.observe(srv.lookup())
	for range 4 {
		r := httptest.NewRequest(http.MethodGet, "/check", http.NoBody)
		r.Header.Set("X-Real-IP", "198.51.100.1")
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}

	clock.advance(90 * time.Second)
	lookup.length = 184213
	srv.readiness.observe(srv.lookup())

	recovered := recordsWithMsg(logRecords(t, logs), "check: blocklist now usable; normal enforcement resumed")
	require.Len(t, recovered, 1)
	require.Equal(t, "INFO", recovered[0]["level"], "recovery is good news, not a fault")
	require.Equal(t, "1m30s", recovered[0]["unusable_for"])
	require.Equal(t, float64(4), recovered[0]["failopen_allowed_total_window"])
	require.Equal(t, float64(184213), recovered[0]["prefixes"])
}

// TestReadiness_WarmStartIsSilent confirms the contract emits nothing at
// all on a normal restart that finds a usable disk cache.
func TestReadiness_WarmStartIsSilent(t *testing.T) {
	srv, logs, clock := testServer(t, config.StartupFailClosed, func() Lookup { return &fakeLookup{length: 42} })

	srv.readiness.observe(srv.lookup())
	clock.advance(heartbeatInterval)
	srv.readiness.heartbeat(srv.lookup())

	require.Empty(t, strings.TrimSpace(logs.String()),
		"a daemon that was never unusable must emit nothing from this contract")
}

// TestHealthz_EmptyForSecondsTracksTheWindow pins the /healthz duration
// field to the injected clock.
func TestHealthz_EmptyForSecondsTracksTheWindow(t *testing.T) {
	srv, _, clock := testServer(t, config.StartupFailOpen, func() Lookup { return nil })
	handler := srv.Handler()

	srv.readiness.observe(srv.lookup())
	clock.advance(3721 * time.Second)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, float64(3721), body["empty_for_seconds"])
	require.Equal(t, "allow-all", body["serving"])
}

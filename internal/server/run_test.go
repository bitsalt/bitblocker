package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/server"
)

// TestRun_LifecycleAgainstLoopback exercises the full Run() path: bind,
// serve a real /healthz request over loopback, and shut down on
// context cancellation. The test takes a free port from the OS so it
// is parallel-safe.
func TestRun_LifecycleAgainstLoopback(t *testing.T) {
	addr := freePort(t)
	stub := &stubLookup{length: 1}

	srv, err := server.New(server.Options{
		Addr:        addr,
		Lookup:      func() server.Lookup { return stub },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BlockStatus: http.StatusForbidden,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	require.Eventually(t, func() bool {
		resp, getErr := http.Get("http://" + addr + "/healthz")
		if getErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 20*time.Millisecond, "server should accept /healthz once Run is up")

	// Verify a check call also works against the live listener.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/check", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Real-IP", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// freePort asks the OS for a free TCP port on the loopback interface
// and returns the host:port string. The listener is closed immediately;
// there is a tiny race window before Run() rebinds, which is acceptable
// in this test.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().(*net.TCPAddr)
	require.NoError(t, l.Close())
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(addr.Port)).String()
}

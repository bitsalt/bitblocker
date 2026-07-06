package fetcher_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/fetcher"
)

// mmdbBytes renders (cidr -> country) pairs into a DB-IP IP-to-Country
// Lite-shaped MMDB and returns the raw bytes. mmdbwriter is pinned to
// v1.0.0 to stay below the Go 1.24 toolchain floor.
func mmdbBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "DBIP-Country-Lite",
		RecordSize:              28,
		IncludeReservedNetworks: true,
	})
	require.NoError(t, err)
	for cidr, country := range entries {
		_, network, perr := net.ParseCIDR(cidr)
		require.NoErrorf(t, perr, "parse fixture cidr %q", cidr)
		require.NoErrorf(t, w.Insert(network, mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String(country)},
		}), "insert fixture %q", cidr)
	}
	var buf bytes.Buffer
	_, err = w.WriteTo(&buf)
	require.NoError(t, err)
	return buf.Bytes()
}

// gzipBytes gzips raw, mirroring DB-IP's single-stream .mmdb.gz format.
func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedClock returns a clock pinned to the given RFC3339 instant.
func fixedClock(t *testing.T, rfc3339 string) func() time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	require.NoError(t, err)
	return func() time.Time { return ts }
}

// capturingHandler records every request it receives (for header
// assertions) and delegates to fn.
type capturingHandler struct {
	mu       sync.Mutex
	requests []*http.Request
	fn       http.HandlerFunc
}

func (h *capturingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.Clone(context.Background()))
	h.mu.Unlock()
	h.fn(w, r)
}

func (h *capturingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func (h *capturingHandler) last() *http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests[len(h.requests)-1]
}

// newFetcher wires a Fetcher against the stub server at baseURL with a
// fixed clock and a temp cache path. It returns the fetcher, its source,
// and the cache path.
func newFetcher(t *testing.T, baseURL, clockAt string) (*fetcher.Fetcher, *blocklist.Source, string) {
	t.Helper()
	src := blocklist.NewSource()
	cachePath := filepath.Join(t.TempDir(), "dbip-country-lite.mmdb")
	f, err := fetcher.New(fetcher.Options{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		CachePath:  cachePath,
		Countries:  []config.CountryCode{"CN"},
		Source:     src,
		Logger:     testLogger(),
		Now:        fixedClock(t, clockAt),
	})
	require.NoError(t, err)
	f.SetBaseURLForTest(baseURL)
	return f, src, cachePath
}

func TestRefresh_HappyPath_FetchesGunzipsCachesAndSwaps(t *testing.T) {
	gz := gzipBytes(t, mmdbBytes(t, map[string]string{
		"10.0.0.0/24":  "CN",
		"192.0.2.0/24": "US",
	}))
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/dbip-country-lite-2026-07.mmdb.gz", r.URL.Path)
		w.Header().Set("ETag", `"july"`)
		_, _ = w.Write(gz)
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, src, cachePath := newFetcher(t, srv.URL, "2026-07-15T00:00:00Z")

	outcome, err := f.Refresh(context.Background())
	require.NoError(t, err)
	require.Equal(t, fetcher.OutcomeUpdated, outcome)

	// The blocklist was swapped in and reflects the fetched data.
	trie := src.Current()
	require.NotNil(t, trie)
	require.Equal(t, 1, trie.Len())
	require.True(t, trie.Contains(netip.MustParseAddr("10.0.0.5")))
	require.False(t, trie.Contains(netip.MustParseAddr("192.0.2.5")))

	// The raw MMDB was written through the disk cache.
	_, statErr := os.Stat(cachePath)
	require.NoError(t, statErr)
}

func TestRefresh_ConditionalGet_304RetainsBlocklist(t *testing.T) {
	gz := gzipBytes(t, mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"}))
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"july"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"july"`)
		_, _ = w.Write(gz)
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, src, _ := newFetcher(t, srv.URL, "2026-07-15T00:00:00Z")

	// First fetch: 200, stores the ETag.
	outcome, err := f.Refresh(context.Background())
	require.NoError(t, err)
	require.Equal(t, fetcher.OutcomeUpdated, outcome)
	first := src.Current()
	require.NotNil(t, first)

	// Second fetch: the daemon sends If-None-Match and the server 304s.
	outcome, err = f.Refresh(context.Background())
	require.NoError(t, err)
	require.Equal(t, fetcher.OutcomeUnchanged, outcome)

	// The active trie is the same object — a 304 does not rebuild or swap.
	require.Same(t, first, src.Current())
	require.Equal(t, `"july"`, h.last().Header.Get("If-None-Match"))
}

func TestRefresh_CurrentMonth404_FallsBackToPriorMonth(t *testing.T) {
	gz := gzipBytes(t, mmdbBytes(t, map[string]string{"10.0.0.0/24": "CN"}))
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dbip-country-lite-2026-07.mmdb.gz":
			http.NotFound(w, r) // current month not published yet
		case "/dbip-country-lite-2026-06.mmdb.gz":
			w.Header().Set("ETag", `"june"`)
			_, _ = w.Write(gz)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// July 2nd: rollover window where the July file may not exist yet.
	f, src, _ := newFetcher(t, srv.URL, "2026-07-02T00:00:00Z")

	outcome, err := f.Refresh(context.Background())
	require.NoError(t, err)
	require.Equal(t, fetcher.OutcomeUpdated, outcome)
	require.Equal(t, 2, h.count(), "should try current then prior month")
	require.NotNil(t, src.Current())
	require.True(t, src.Current().Contains(netip.MustParseAddr("10.0.0.5")))
}

func TestRefresh_BothMonths404_ReturnsErrNotPublishedAndRetains(t *testing.T) {
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, src, _ := newFetcher(t, srv.URL, "2026-07-02T00:00:00Z")

	outcome, err := f.Refresh(context.Background())
	require.ErrorIs(t, err, fetcher.ErrNotPublished)
	require.Equal(t, fetcher.OutcomeUnchanged, outcome)
	require.Equal(t, 2, h.count())
	require.Nil(t, src.Current(), "a rollover-day double-404 must not clobber the (empty) blocklist")
}

func TestRefresh_CorruptGzip_ErrorsWithoutSwappingOrPoisoningCache(t *testing.T) {
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"july"`)
		_, _ = w.Write([]byte("this is not a gzip stream"))
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, src, cachePath := newFetcher(t, srv.URL, "2026-07-15T00:00:00Z")

	outcome, err := f.Refresh(context.Background())
	require.Error(t, err)
	require.Equal(t, fetcher.OutcomeUnchanged, outcome)
	require.Nil(t, src.Current(), "a corrupt download must not swap in a blocklist")

	// The corrupt bytes never reached the cache write (gunzip failed first).
	_, statErr := os.Stat(cachePath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRefresh_UnexpectedStatus_Errors(t *testing.T) {
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, src, _ := newFetcher(t, srv.URL, "2026-07-15T00:00:00Z")

	_, err := f.Refresh(context.Background())
	require.Error(t, err)
	require.Nil(t, src.Current())
}

func TestRefresh_ContextCancellation_Errors(t *testing.T) {
	h := &capturingHandler{fn: func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the client cancels
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	f, _, _ := newFetcher(t, srv.URL, "2026-07-15T00:00:00Z")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.Refresh(ctx)
	require.Error(t, err)
}

package fetcher

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/diskcache"
	"github.com/bitsalt/bitblocker/internal/mmdb"
)

const (
	// defaultBaseURL is the DB-IP free-download host and path prefix.
	// The month-stamped filename is appended at fetch time. Tests
	// override this (via the export_test seam) to point at a stub
	// httptest server; it is deliberately not a config field (ADR 0003).
	defaultBaseURL = "https://download.db-ip.com/free"

	// urlFilenameFormat renders the month-stamped DB-IP filename. The
	// verb is the "YYYY-MM" month segment.
	urlFilenameFormat = "dbip-country-lite-%s.mmdb.gz"

	// monthLayout is the UTC "YYYY-MM" layout DB-IP stamps into filenames.
	monthLayout = "2006-01"

	// maxDecompressedBytes caps gunzip output to defend against a
	// decompression bomb. DB-IP IP-to-Country Lite is ~30 MB
	// uncompressed as of mid-2026; 512 MB is generous headroom.
	maxDecompressedBytes = 512 << 20
)

// ErrNotPublished is returned when neither the current nor the prior
// month's file is available (both return 404). The daemon treats this as
// a transient fetch failure: it retains the active blocklist and the
// retry budget / cron applies. It is not fatal on a month-rollover day.
var ErrNotPublished = errors.New("fetcher: no published file for current or prior month")

// Outcome describes what a Refresh accomplished, for the caller's logs.
type Outcome int

const (
	// OutcomeUpdated means a new MMDB was fetched, cached, and swapped in.
	OutcomeUpdated Outcome = iota
	// OutcomeUnchanged means the server reported the file unchanged (304);
	// the active blocklist is already current and was not touched.
	OutcomeUnchanged
)

// Fetcher refreshes the active blocklist from DB-IP. It is safe for
// serialized callers: Refresh takes an internal lock so a cron tick that
// overlaps a still-running refresh cannot race the conditional-GET
// validators. See ADR 0003.
type Fetcher struct {
	httpClient *http.Client
	baseURL    string
	now        func() time.Time
	logger     *slog.Logger

	cachePath string
	countries []config.CountryCode
	source    *blocklist.Source

	// mu serializes Refresh and guards the conditional-GET validators
	// below, which are read and written across cron goroutines.
	mu           sync.Mutex
	etag         string
	lastModified string
}

// Options configures a Fetcher. All fields except Now are required.
type Options struct {
	// HTTPClient issues the download GETs. The caller sets its Timeout;
	// per-fetch cancellation additionally flows through the context.
	HTTPClient *http.Client
	// CachePath is the on-disk MMDB snapshot path (config cache.path).
	CachePath string
	// Countries is the block set the loader filters the MMDB against.
	Countries []config.CountryCode
	// Source is the atomic blocklist publisher a successful fetch swaps.
	Source *blocklist.Source
	// Logger receives the fetcher's operational events.
	Logger *slog.Logger
	// Now supplies the current time for month derivation. Defaults to
	// time.Now when nil; tests inject a fixed clock.
	Now func() time.Time
}

// New constructs a Fetcher from opts, validating required fields.
func New(opts Options) (*Fetcher, error) {
	if opts.HTTPClient == nil {
		return nil, errors.New("fetcher: HTTPClient is required")
	}
	if opts.CachePath == "" {
		return nil, errors.New("fetcher: CachePath is required")
	}
	if len(opts.Countries) == 0 {
		return nil, errors.New("fetcher: Countries is required")
	}
	if opts.Source == nil {
		return nil, errors.New("fetcher: Source is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("fetcher: Logger is required")
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Fetcher{
		httpClient: opts.HTTPClient,
		baseURL:    defaultBaseURL,
		now:        now,
		logger:     opts.Logger,
		cachePath:  opts.CachePath,
		countries:  opts.Countries,
		source:     opts.Source,
	}, nil
}

// Refresh fetches the current month's DB-IP file (falling back to the
// prior month on a rollover-day 404), and on a changed file writes it
// through the disk cache, rebuilds the trie, and swaps it into the active
// blocklist. A 304 short-circuits before any download or swap.
//
// On any fetch failure — network error, both months 404, a corrupt gzip
// stream — Refresh returns an error and leaves the active blocklist
// untouched, so the caller can retain the prior ruleset and retry.
func (f *Fetcher) Refresh(ctx context.Context) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, notModified, v, err := f.fetch(ctx)
	if err != nil {
		return OutcomeUnchanged, err
	}
	if notModified {
		f.logger.Debug("fetcher: source unchanged (304); retaining active blocklist")
		return OutcomeUnchanged, nil
	}

	// The gzip stream has already been fully decompressed and its CRC
	// verified by fetch, so a truncated download fails before we reach
	// this write and never overwrites a good cache. Write-then-load is
	// the ADR 0002/0003 prescribed order: the disk cache is the one
	// artifact, and mmdb.LoadCountryBlocklist is the one MMDB->trie path.
	if err = diskcache.Write(f.cachePath, bytes.NewReader(body)); err != nil {
		return OutcomeUnchanged, fmt.Errorf("fetcher: write cache: %w", err)
	}
	trie, err := mmdb.LoadCountryBlocklist(f.cachePath, f.countries)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("fetcher: load fetched mmdb: %w", err)
	}

	f.source.Swap(trie)
	f.etag = v.etag
	f.lastModified = v.lastModified
	f.logger.Info("fetcher: blocklist refreshed", "prefixes", trie.Len())
	return OutcomeUpdated, nil
}

// validators carries the conditional-GET response headers to remember
// for the next request.
type validators struct {
	etag         string
	lastModified string
}

// fetch performs the conditional GET for the current month, falling back
// to the prior month on a 404. It returns the decompressed MMDB bytes
// (nil when notModified), whether the server reported 304, and the
// validators to store on a successful download.
func (f *Fetcher) fetch(ctx context.Context) (body []byte, notModified bool, v validators, err error) {
	now := f.now().UTC()
	current := now.Format(monthLayout)
	prior := now.AddDate(0, 0, -now.Day()).Format(monthLayout) // last day of prior month

	body, notModified, v, status, err := f.get(ctx, current)
	if err != nil {
		return nil, false, validators{}, err
	}
	if status == http.StatusNotFound {
		f.logger.Warn("fetcher: current-month file not published; trying prior month",
			"month", current, "prior_month", prior)
		body, notModified, v, status, err = f.get(ctx, prior)
		if err != nil {
			return nil, false, validators{}, err
		}
		if status == http.StatusNotFound {
			return nil, false, validators{}, fmt.Errorf("%w (%s, %s)", ErrNotPublished, current, prior)
		}
	}
	return body, notModified, v, nil
}

// get issues one conditional GET for the given month and classifies the
// response. A 200 returns the decompressed bytes and fresh validators; a
// 304 returns notModified with the existing validators; a 404 returns the
// status for the caller's rollover fallback; any other status is an error.
func (f *Fetcher) get(ctx context.Context, month string) (body []byte, notModified bool, v validators, status int, err error) {
	url := fmt.Sprintf("%s/"+urlFilenameFormat, f.baseURL, month)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, false, validators{}, 0, fmt.Errorf("fetcher: build request for %s: %w", month, err)
	}
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}
	if f.lastModified != "" {
		req.Header.Set("If-Modified-Since", f.lastModified)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, false, validators{}, 0, fmt.Errorf("fetcher: GET %s: %w", month, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		data, derr := decompress(resp.Body)
		if derr != nil {
			return nil, false, validators{}, resp.StatusCode, derr
		}
		return data, false, validators{
			etag:         resp.Header.Get("ETag"),
			lastModified: resp.Header.Get("Last-Modified"),
		}, resp.StatusCode, nil
	case http.StatusNotModified:
		drain(resp.Body)
		return nil, true, validators{etag: f.etag, lastModified: f.lastModified}, resp.StatusCode, nil
	case http.StatusNotFound:
		drain(resp.Body)
		return nil, false, validators{}, resp.StatusCode, nil
	default:
		drain(resp.Body)
		return nil, false, validators{}, resp.StatusCode, fmt.Errorf("fetcher: GET %s: unexpected status %d", month, resp.StatusCode)
	}
}

// decompress gunzips a single-stream gzip body into bytes, bounded
// against a decompression bomb. The gzip reader validates the stream's
// CRC and length trailer at EOF, so a truncated or corrupt download
// surfaces here as an error before any cache write.
func decompress(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("fetcher: open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	// +1 lets us distinguish "exactly at the cap" from "over the cap".
	data, err := io.ReadAll(io.LimitReader(gz, maxDecompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetcher: decompress: %w", err)
	}
	if int64(len(data)) > maxDecompressedBytes {
		return nil, fmt.Errorf("fetcher: decompressed stream exceeds %d-byte cap", maxDecompressedBytes)
	}
	return data, nil
}

// drain reads and discards a response body so the underlying connection
// can be reused for the next conditional request.
func drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}

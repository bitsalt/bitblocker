package diskcache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bitsalt/bitblocker/internal/blocklist"
	"github.com/bitsalt/bitblocker/internal/config"
	"github.com/bitsalt/bitblocker/internal/mmdb"
)

// ErrAbsent is returned by Load when no cache file exists at the
// configured path. It is an expected control-flow signal — the daemon
// proceeds with a cold start — not an error to log at ERROR.
var ErrAbsent = errors.New("diskcache: snapshot not present")

// ErrStale is returned by Load when the cache file exists but its
// modification time is older than the configured max age. It is an
// expected control-flow signal: the caller logs WARN and skips the
// cache.
var ErrStale = errors.New("diskcache: snapshot exceeds max age")

// Write copies the MMDB bytes in src to path, crash-safely. The
// blocklist trie is rebuilt from this exact file on the next startup
// (see Load), so the cached artifact is the raw MaxMind MMDB — there is
// no derived format. See ADR 0002.
//
// The write uses the standard temp-file-plus-rename discipline: bytes
// land in a temp file in the same directory as path (same filesystem,
// so the rename is atomic), the temp file is Synced before close, then
// renamed over path. A crash at any point leaves either the prior
// cache or no cache — never a half-written file.
//
// Failure is non-fatal to the caller: a daemon that cannot write the
// cache logs WARN and continues, because the in-memory trie is already
// serving. The cache is an optimization for the next start.
func Write(path string, src io.Reader) (err error) {
	dir := filepath.Dir(path)
	// 0o755 per interface spec §4.3 / ADR 0002 §B: a cache directory
	// under /var/cache is FHS-conventional world-readable; it holds
	// only the public MaxMind MMDB, no secrets. In production the
	// systemd unit's CacheDirectory= owns creation and ownership.
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil { //nolint:gosec // G301: see comment above
		return fmt.Errorf("diskcache: create dir %q: %w", dir, mkErr)
	}

	tmp, err := os.CreateTemp(dir, "*.mmdb.tmp")
	if err != nil {
		return fmt.Errorf("diskcache: create temp file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()

	// On any pre-rename failure, remove the temp file. After a
	// successful rename the temp file no longer exists, err is nil,
	// and this is a no-op.
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = io.Copy(tmp, src); err != nil {
		return fmt.Errorf("diskcache: write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("diskcache: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("diskcache: close temp file: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("diskcache: rename into place: %w", err)
	}
	return nil
}

// Load reads the disk cache at path and rebuilds a *blocklist.Trie from
// it, subject to a staleness bound. The cache is always a head start
// ahead of the first network fetch, never a substitute for one. See
// ADR 0002.
//
// now is injected so the staleness check is testable; callers pass
// time.Now() at the wiring seam.
//
// The return discriminates four outcomes via errors.Is at the caller:
//   - ErrAbsent — no file at path; proceed with a cold start.
//   - ErrStale — file older than maxAge; skip it.
//   - any other non-nil error — the file is unreadable or a corrupt
//     MMDB; skip it (the caller logs WARN and removes the file).
//   - nil error — the trie is returned. A trie with Len() == 0 is a
//     success: the server's existing Len() == 0 check keeps the daemon
//     fail-closed until the fetcher delivers a populated trie.
func Load(path string, maxAge time.Duration, now time.Time, countries []config.CountryCode) (*blocklist.Trie, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAbsent
		}
		return nil, fmt.Errorf("diskcache: stat %q: %w", path, err)
	}

	if age := now.Sub(info.ModTime()); age > maxAge {
		return nil, ErrStale
	}

	// Load is the validation step: a truncated or corrupt MMDB fails
	// LoadCountryBlocklist's open or decode, which is the only
	// integrity check the cache needs (ADR 0002 §C).
	trie, err := mmdb.LoadCountryBlocklist(path, countries)
	if err != nil {
		return nil, fmt.Errorf("diskcache: load %q: %w", path, err)
	}
	return trie, nil
}

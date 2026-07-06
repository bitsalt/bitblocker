// Package scheduler decides *when* the daemon refreshes its blocklist.
// It owns two timing concerns and no data logic of its own: a periodic
// cron (refresh.schedule) that re-fetches on a fixed cadence, and a
// bounded cold-start retry budget that, on a startup with no usable
// blocklist, retries the fetch on exponential backoff until the first
// success — keeping the daemon fail-closed until then.
//
// The scheduler is driven by an injected RefreshFunc; it does not know
// how a refresh is performed (that is the fetcher's job). This keeps the
// timing policy testable without a network and the fetch logic testable
// without a clock. See ADR 0003 and docs/BitBlocker.md Sprint 3.
package scheduler

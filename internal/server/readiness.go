package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitsalt/bitblocker/internal/config"
)

// heartbeatInterval is how often the daemon re-reports a persistent
// unusable-blocklist state. It is deliberately a constant rather than a
// config field (ADR 0004 §Consequences/Neutral, OQ-FAILOPEN-2): a daemon
// that has been inert for a week should still be shouting once a minute,
// and an operator who chose fail-open must not be able to quiet it.
const heartbeatInterval = 60 * time.Second

// serving posture strings reported on /healthz and in the readiness log
// signals. They make the daemon's current behavior externally legible
// without the reader having to correlate startup_mode against the
// blocklist state.
const (
	servingEnforcing = "enforcing"
	servingDenyAll   = "deny-all"
	servingAllowAll  = "allow-all"
)

// likely_cause values for the heartbeat, keyed on ever_ready. The two
// unusable sub-states have completely different fixes, so the daemon
// names the one it is in rather than leaving the operator to infer it
// (ADR 0004 §A.4).
const (
	causeNeverReady     = "no successful fetch since start"
	causeReadyThenEmpty = "blocklist loaded but matched no configured countries — check block.countries"
)

// usable reports whether l can serve authorization decisions. This is
// the single readiness predicate: /check, /healthz, and the readiness
// tracker all route through it so they cannot drift apart.
//
// A nil Lookup means no swap has published a trie yet; a zero-length one
// means a trie was published but matched nothing.
func usable(l Lookup) bool {
	return l != nil && l.Len() > 0
}

// readiness turns the instantaneous usable/unusable predicate into the
// durable signals an operator needs: a once-per-transition ERROR on
// entering the unusable state, a recurring ERROR heartbeat while it
// persists, and an INFO on recovery. See ADR 0004 §D and
// docs/interfaces/fail-open-and-readiness.md §4.
//
// A silently-allow-all daemon is indistinguishable from a healthy one,
// so these signals are the larger half of the fail-open feature rather
// than an accompaniment to it.
//
// All state is read from many request goroutines plus the heartbeat
// goroutine. Counters and the current-state flags are atomic; the
// transition path is serialized under mu so a transition emits exactly
// once even when many requests observe the change concurrently.
type readiness struct {
	mu sync.Mutex

	// usable mirrors the last observed predicate value. It starts true
	// so that a cold start (which observes unusable) registers as a
	// transition and emits the entering signal, while a warm start with
	// a populated cache emits nothing at all.
	usable atomic.Bool
	// everReady latches on the first usable observation and is never
	// reset. False on a long-running daemon means it has never
	// functioned — the dead-daemon case.
	everReady atomic.Bool
	// unusableSince is the unix-nano start of the current unusable
	// window, zero while usable.
	unusableSince atomic.Int64
	// failOpenTotal counts every request allowed because the blocklist
	// was unusable under fail-open, since process start.
	failOpenTotal atomic.Uint64
	// failOpenSinceHeartbeat counts the same since the last heartbeat
	// emission, and is reset by it.
	failOpenSinceHeartbeat atomic.Uint64
	// failOpenAtWindowStart snapshots failOpenTotal when the current
	// unusable window opened, so the recovery signal can report how many
	// requests that window let through.
	failOpenAtWindowStart atomic.Uint64

	now    func() time.Time
	logger *slog.Logger
	mode   config.StartupMode
}

// newReadiness builds a tracker for the given startup mode. now is
// injected so window durations are testable without wall time.
func newReadiness(mode config.StartupMode, logger *slog.Logger, now func() time.Time) *readiness {
	t := &readiness{now: now, logger: logger, mode: mode}
	t.usable.Store(true)
	return t
}

// failOpen reports whether an unusable blocklist should allow requests
// through rather than deny them.
func (t *readiness) failOpen() bool {
	return t.mode == config.StartupFailOpen
}

// serving describes what the daemon is currently doing with /check
// traffic, given the readiness predicate.
func (t *readiness) serving(u bool) string {
	switch {
	case u:
		return servingEnforcing
	case t.failOpen():
		return servingAllowAll
	default:
		return servingDenyAll
	}
}

// observe records the current readiness predicate and emits the
// transition signals when it changes. It returns the predicate so
// callers can branch on it without re-reading the Lookup — the handlers
// must evaluate a single s.lookup() result per request, since a
// concurrent Swap between two reads would let one request see two
// different tries.
func (t *readiness) observe(l Lookup) bool {
	u := usable(l)

	if u && !t.everReady.Load() {
		t.everReady.Store(true)
	}
	if t.usable.Load() == u {
		return u
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// Re-check under the lock: a concurrent observer may have handled
	// this same transition already, and it must be emitted only once.
	if t.usable.Load() == u {
		return u
	}

	if u {
		t.emitRecovered(l)
	} else {
		t.emitUnusable()
	}
	t.usable.Store(u)
	return u
}

// emitUnusable reports entry into the unusable state. Called under mu.
//
// The level is ERROR under both startup modes: a daemon that cannot make
// authorization decisions has failed at its only job, whether it is
// denying everything or allowing everything.
func (t *readiness) emitUnusable() {
	t.unusableSince.Store(t.now().UnixNano())
	t.failOpenAtWindowStart.Store(t.failOpenTotal.Load())
	t.failOpenSinceHeartbeat.Store(0)

	msg := "check: blocklist unusable; denying all requests"
	if t.failOpen() {
		msg = "check: blocklist unusable; ALLOWING ALL REQUESTS (startup_mode=fail-open)"
	}
	t.logger.Error(msg,
		"startup_mode", string(t.mode),
		"ever_ready", t.everReady.Load(),
		"serving", t.serving(false),
	)
}

// emitRecovered reports the end of an unusable window. Called under mu.
// Recovery is good news, so it is INFO rather than ERROR.
func (t *readiness) emitRecovered(l Lookup) {
	attrs := []any{
		"unusable_for", t.windowDuration(),
		"prefixes", l.Len(),
	}
	if t.failOpen() {
		allowed := t.failOpenTotal.Load() - t.failOpenAtWindowStart.Load()
		attrs = append(attrs, "failopen_allowed_total_window", allowed)
	}
	t.unusableSince.Store(0)
	t.logger.Info("check: blocklist now usable; normal enforcement resumed", attrs...)
}

// heartbeat re-observes the predicate and, if the blocklist is still
// unusable, emits the recurring ERROR. Observing first means a recovery
// that happened while no requests arrived is still reported.
//
// This signal is not suppressible: it is gated on neither
// behavior.log_blocked nor behavior.log_allowed, and logging.level tops
// out at error, so no valid config can silence it (ADR 0004 §E).
func (t *readiness) heartbeat(l Lookup) {
	if t.observe(l) {
		return
	}

	everReady := t.everReady.Load()
	cause := causeNeverReady
	if everReady {
		cause = causeReadyThenEmpty
	}

	attrs := []any{
		"startup_mode", string(t.mode),
		"serving", t.serving(false),
		"ever_ready", everReady,
		"unusable_for", t.windowDuration(),
	}
	if t.failOpen() {
		attrs = append(attrs,
			"failopen_allowed_total", t.failOpenTotal.Load(),
			"failopen_allowed_since_last", t.failOpenSinceHeartbeat.Swap(0),
		)
	}
	attrs = append(attrs, "likely_cause", cause)

	t.logger.Error("check: blocklist still unusable", attrs...)
}

// countFailOpenAllow records a request allowed only because the
// blocklist was unusable under fail-open. These counters are what let
// the heartbeat and the recovery signal report what got through, instead
// of logging every such request and flooding the log during exactly the
// incident an operator needs to read it.
func (t *readiness) countFailOpenAllow() {
	t.failOpenTotal.Add(1)
	t.failOpenSinceHeartbeat.Add(1)
}

// windowDuration renders how long the current unusable window has run,
// rounded to the second. Returns "0s" when the blocklist is usable.
func (t *readiness) windowDuration() string {
	return t.windowSince().Round(time.Second).String()
}

// windowSince returns the elapsed time in the current unusable window,
// or zero when the blocklist is usable.
func (t *readiness) windowSince() time.Duration {
	start := t.unusableSince.Load()
	if start == 0 {
		return 0
	}
	return t.now().Sub(time.Unix(0, start))
}

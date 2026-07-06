package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// Cold-start retry-budget defaults. The budget is bounded: after the
// last attempt the daemon stays fail-closed and waits for the next
// scheduled (cron) refresh rather than retrying forever. These are not
// config fields — the operator tunes cadence via refresh.schedule; the
// cold-start budget is an implementation detail of surviving a transient
// startup outage.
const (
	defaultColdStartMaxAttempts = 8
	defaultColdStartBaseDelay   = 2 * time.Second
	defaultColdStartMaxDelay    = 5 * time.Minute
)

// RefreshFunc performs a single blocklist refresh. It returns nil on
// success (a new ruleset was swapped in, or the source was already
// current) and an error on a fetch failure, after which the caller
// retains the prior blocklist.
type RefreshFunc func(ctx context.Context) error

// RetryPolicy bounds the cold-start retry budget.
type RetryPolicy struct {
	// MaxAttempts caps how many times the cold start retries before
	// giving up and deferring to the cron cadence.
	MaxAttempts int
	// BaseDelay is the first backoff interval; each attempt doubles it.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultColdStartMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultColdStartBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultColdStartMaxDelay
	}
	return p
}

// sleepFunc waits for d or until ctx is cancelled, returning ctx.Err()
// if cancelled. Injected so cold-start backoff is testable without wall
// time.
type sleepFunc func(ctx context.Context, d time.Duration) error

// Scheduler runs the periodic refresh cron and the cold-start retry
// budget. Construct with New; drive with Run.
type Scheduler struct {
	schedule  string
	timeout   time.Duration
	refresh   RefreshFunc
	ready     func() bool
	logger    *slog.Logger
	coldStart RetryPolicy
	sleep     sleepFunc
}

// Options configures a Scheduler. All fields except ColdStart are
// required; ColdStart's zero values fall back to the package defaults.
type Options struct {
	// Schedule is the cron expression (refresh.schedule). It must already
	// have passed config validation.
	Schedule string
	// Timeout bounds each individual refresh (refresh.timeout).
	Timeout time.Duration
	// Refresh performs one refresh. Required.
	Refresh RefreshFunc
	// Ready reports whether a usable blocklist is already loaded (e.g.
	// from the disk cache). When true, a failed initial refresh does not
	// burn the cold-start budget — the cached ruleset serves until the
	// next scheduled refresh. Required.
	Ready func() bool
	// Logger receives the scheduler's operational events. Required.
	Logger *slog.Logger
	// ColdStart tunes the startup retry budget. Zero values use defaults.
	ColdStart RetryPolicy
}

// New constructs a Scheduler from opts using a real clock for backoff.
func New(opts Options) (*Scheduler, error) {
	return newScheduler(opts, realSleep)
}

func newScheduler(opts Options, sleep sleepFunc) (*Scheduler, error) {
	if opts.Schedule == "" {
		return nil, errors.New("scheduler: Schedule is required")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("scheduler: Timeout must be positive")
	}
	if opts.Refresh == nil {
		return nil, errors.New("scheduler: Refresh is required")
	}
	if opts.Ready == nil {
		return nil, errors.New("scheduler: Ready is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("scheduler: Logger is required")
	}
	return &Scheduler{
		schedule:  opts.Schedule,
		timeout:   opts.Timeout,
		refresh:   opts.Refresh,
		ready:     opts.Ready,
		logger:    opts.Logger,
		coldStart: opts.ColdStart.withDefaults(),
		sleep:     sleep,
	}, nil
}

// Run performs the cold-start refresh (with retry budget), then starts
// the periodic cron and blocks until ctx is cancelled, draining any
// in-flight scheduled refresh before returning.
func (s *Scheduler) Run(ctx context.Context) error {
	s.runColdStart(ctx)

	if ctx.Err() != nil {
		return nil
	}

	// SkipIfStillRunning: a scheduled tick that overlaps a still-running
	// refresh is skipped rather than queued — harmless for a daily cron
	// over a monthly file, and it keeps the conditional-GET validators
	// from being touched concurrently.
	c := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	if _, err := c.AddFunc(s.schedule, func() {
		if err := s.refreshOnce(ctx); err != nil {
			s.logger.Warn("scheduler: scheduled refresh failed; retaining active blocklist", "error", err)
		}
	}); err != nil {
		// Unreachable in practice: config validation already parsed the
		// schedule. Defensive so a future config path cannot start a
		// daemon with a silently dead cron.
		return fmt.Errorf("scheduler: add cron job %q: %w", s.schedule, err)
	}

	c.Start()
	s.logger.Info("scheduler: started", "schedule", s.schedule)

	<-ctx.Done()
	<-c.Stop().Done()
	s.logger.Info("scheduler: stopped")
	return nil
}

// runColdStart attempts the first refresh, retrying on a bounded
// exponential backoff while no usable blocklist is loaded. If a cached
// blocklist is already active, a single failure defers to the cron
// cadence instead of burning the budget.
func (s *Scheduler) runColdStart(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}

		err := s.refreshOnce(ctx)
		if err == nil {
			return
		}

		if s.ready() {
			s.logger.Warn("scheduler: initial refresh failed but a cached blocklist is active; relying on it until the next scheduled refresh",
				"error", err)
			return
		}

		if attempt+1 >= s.coldStart.MaxAttempts {
			s.logger.Error("scheduler: cold-start retry budget exhausted; daemon stays fail-closed until the next scheduled refresh",
				"attempts", attempt+1, "error", err)
			return
		}

		delay := backoff(s.coldStart, attempt)
		s.logger.Warn("scheduler: cold-start fetch failed; retrying",
			"attempt", attempt+1, "retry_in", delay.String(), "error", err)
		if serr := s.sleep(ctx, delay); serr != nil {
			return // context cancelled during backoff
		}
	}
}

// refreshOnce runs a single refresh under the per-fetch timeout.
func (s *Scheduler) refreshOnce(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.refresh(fctx)
}

// backoff returns BaseDelay * 2^attempt, capped at MaxDelay. attempt is
// always non-negative (the cold-start loop starts at 0) and bounded by
// MaxAttempts; the >=62 guard and the d<=0 overflow check together keep
// the shift from wrapping to a non-positive duration.
func backoff(p RetryPolicy, attempt int) time.Duration {
	if attempt >= 62 {
		return p.MaxDelay
	}
	d := p.BaseDelay << attempt
	if d <= 0 || d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// realSleep waits for d or ctx cancellation, whichever comes first.
func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

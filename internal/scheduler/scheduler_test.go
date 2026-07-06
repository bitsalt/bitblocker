package scheduler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/scheduler"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingSleep returns a sleep func that records the delays it was
// asked to wait and returns immediately (no wall-clock delay). It honors
// context cancellation.
func recordingSleep() (func(ctx context.Context, d time.Duration) error, *[]time.Duration, *sync.Mutex) {
	var mu sync.Mutex
	var delays []time.Duration
	fn := func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}
	return fn, &delays, &mu
}

func baseOptions(refresh scheduler.RefreshFunc, ready func() bool) scheduler.Options {
	return scheduler.Options{
		Schedule: "0 3 * * *",
		Timeout:  time.Second,
		Refresh:  refresh,
		Ready:    ready,
		Logger:   testLogger(),
	}
}

func TestNew_ValidatesRequiredFields(t *testing.T) {
	_, err := scheduler.New(scheduler.Options{})
	require.Error(t, err)

	ok := baseOptions(func(context.Context) error { return nil }, func() bool { return false })
	_, err = scheduler.New(ok)
	require.NoError(t, err)
}

func TestColdStart_SucceedsFirstAttempt_NoRetry(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return nil
	}
	sleep, delays, mu := recordingSleep()

	s, err := scheduler.NewForTest(baseOptions(refresh, func() bool { return false }), sleep)
	require.NoError(t, err)

	// Cancel the context right after cold start so Run does not block on
	// the (never-firing) daily cron.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Give cold start a moment, then stop the cron loop.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	require.NoError(t, s.Run(ctx))

	require.Equal(t, int32(1), calls.Load())
	mu.Lock()
	require.Empty(t, *delays, "a first-attempt success must not sleep")
	mu.Unlock()
}

func TestColdStart_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("boom")
		}
		return nil // succeed on the third attempt
	}
	sleep, delays, mu := recordingSleep()

	opts := baseOptions(refresh, func() bool { return false })
	opts.ColdStart = scheduler.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: time.Minute}
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	require.NoError(t, s.Run(ctx))

	require.Equal(t, int32(3), calls.Load(), "should retry until the third attempt succeeds")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []time.Duration{time.Second, 2 * time.Second}, *delays,
		"backoff should double between attempts")
}

func TestColdStart_BudgetExhausted_DefersToCron(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return errors.New("always fails")
	}
	sleep, delays, mu := recordingSleep()

	opts := baseOptions(refresh, func() bool { return false })
	opts.ColdStart = scheduler.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Minute}
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	require.NoError(t, s.Run(ctx))

	require.Equal(t, int32(3), calls.Load(), "should try exactly MaxAttempts times")
	mu.Lock()
	require.Len(t, *delays, 2, "MaxAttempts attempts sleep MaxAttempts-1 times")
	mu.Unlock()
}

func TestColdStart_ReadyBlocklist_DoesNotBurnBudget(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return errors.New("fetch down")
	}
	sleep, delays, mu := recordingSleep()

	// A cached blocklist is already active.
	opts := baseOptions(refresh, func() bool { return true })
	opts.ColdStart = scheduler.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: time.Minute}
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	require.NoError(t, s.Run(ctx))

	require.Equal(t, int32(1), calls.Load(), "a single failure defers to cron when a cache is present")
	mu.Lock()
	require.Empty(t, *delays)
	mu.Unlock()
}

func TestColdStart_BackoffCapsAtMaxDelay(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return errors.New("down")
	}
	sleep, delays, mu := recordingSleep()

	opts := baseOptions(refresh, func() bool { return false })
	// BaseDelay 1s doubling would reach 8s on the 4th sleep, but MaxDelay
	// caps it at 5s.
	opts.ColdStart = scheduler.RetryPolicy{MaxAttempts: 6, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	require.NoError(t, s.Run(ctx))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second,
	}, *delays)
}

func TestRefresh_RunsUnderPerFetchTimeout(t *testing.T) {
	var sawDeadline atomic.Bool
	refresh := func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			sawDeadline.Store(true)
		}
		return nil
	}
	sleep, _, _ := recordingSleep()

	opts := baseOptions(refresh, func() bool { return false })
	opts.Timeout = 30 * time.Second
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	require.NoError(t, s.Run(ctx))

	require.True(t, sawDeadline.Load(), "each refresh must run under the per-fetch timeout")
}

func TestColdStart_ContextCancelledDuringBackoff_Stops(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return errors.New("down")
	}

	ctx, cancel := context.WithCancel(context.Background())
	// The first backoff cancels the daemon context and reports the
	// cancellation — modeling a SIGTERM arriving mid-retry.
	cancelDuringBackoff := func(bctx context.Context, d time.Duration) error {
		cancel()
		return bctx.Err()
	}

	opts := baseOptions(refresh, func() bool { return false })
	opts.ColdStart = scheduler.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: time.Minute}
	s, err := scheduler.NewForTest(opts, cancelDuringBackoff)
	require.NoError(t, err)

	require.NoError(t, s.Run(ctx))

	require.Equal(t, int32(1), calls.Load(),
		"cold start stops after the first attempt when backoff observes cancellation")
}

func TestCron_PeriodicRefreshFires(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context) error {
		calls.Add(1)
		return nil
	}
	sleep, _, _ := recordingSleep()

	opts := baseOptions(refresh, func() bool { return false })
	// robfig's @every rounds sub-second delays up to 1s, so 1s is the
	// fastest cadence a test can drive the real cron at.
	opts.Schedule = "@every 1s"
	s, err := scheduler.NewForTest(opts, sleep)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2300*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Run(ctx))

	// 1 cold-start call + at least 1 cron tick within ~2.3s.
	require.GreaterOrEqual(t, calls.Load(), int32(2),
		"the periodic cron should fire after cold start")
}

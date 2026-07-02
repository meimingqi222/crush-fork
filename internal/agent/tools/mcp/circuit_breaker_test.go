package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCircuitBreakerState_RecordFailure verifies the pure breaker logic:
// the breaker stays closed below the threshold, opens once the threshold
// is reached within the window, and resets when a failure is recorded
// after the window has elapsed.
func TestCircuitBreakerState_RecordFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	const window = 30 * time.Second
	const threshold = 5

	var s circuitBreakerState
	require.False(t, s.open)

	// Four failures must not open the breaker.
	for i := 0; i < 4; i++ {
		s = s.recordFailure(now, window, threshold)
		require.False(t, s.open, "breaker should not open after %d failures", i+1)
		require.Equal(t, i+1, s.failures)
	}

	// The fifth failure trips the breaker.
	s = s.recordFailure(now, window, threshold)
	require.True(t, s.open)
	require.Equal(t, 5, s.failures)

	// A failure after the window elapses resets the count and closes the
	// breaker.
	later := now.Add(window + time.Second)
	s = s.recordFailure(later, window, threshold)
	require.False(t, s.open, "breaker should reset after window elapses")
	require.Equal(t, 1, s.failures)
}

// TestCircuitBreakerState_IsOpen verifies that isOpen reports true only
// while the breaker is open and within the rolling window.
func TestCircuitBreakerState_IsOpen(t *testing.T) {
	t.Parallel()

	const window = 30 * time.Second
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	// A fresh state is closed.
	require.False(t, circuitBreakerState{}.isOpen(now, window))

	// An open breaker within the window is reported as open.
	open := circuitBreakerState{failures: 5, windowStart: now, open: true}
	require.True(t, open.isOpen(now.Add(10*time.Second), window))

	// Once the window elapses the breaker is treated as closed so
	// auto-reconnect can resume.
	require.False(t, open.isOpen(now.Add(window+time.Second), window))

	// A closed breaker with prior failures is still closed.
	closed := circuitBreakerState{failures: 3, windowStart: now, open: false}
	require.False(t, closed.isOpen(now, window))
}

// TestRecordCircuitBreakerFailure_OpensAfterThreshold verifies the
// package-level helper that records failures against the shared
// circuitBreakers map. After circuitBreakerThreshold failures within the
// window, the breaker opens.
func TestRecordCircuitBreakerFailure_OpensAfterThreshold(t *testing.T) {
	const name = "cb-threshold-test"
	t.Cleanup(func() { circuitBreakers.Del(name) })

	for i := 0; i < circuitBreakerThreshold-1; i++ {
		opened := recordCircuitBreakerFailure(name)
		require.False(t, opened, "breaker should not open after %d failures", i+1)
	}

	// The threshold-th failure opens the breaker.
	opened := recordCircuitBreakerFailure(name)
	require.True(t, opened)
	require.True(t, isCircuitBreakerOpen(name))
}

// TestResetCircuitBreaker_ClearsState verifies that resetCircuitBreaker
// removes the breaker state so isCircuitBreakerOpen reports false.
func TestResetCircuitBreaker_ClearsState(t *testing.T) {
	const name = "cb-reset-test"
	t.Cleanup(func() { circuitBreakers.Del(name) })

	// Trip the breaker.
	for i := 0; i < circuitBreakerThreshold; i++ {
		recordCircuitBreakerFailure(name)
	}
	require.True(t, isCircuitBreakerOpen(name))

	// Reset clears the state.
	resetCircuitBreaker(name)
	require.False(t, isCircuitBreakerOpen(name))

	// A single failure after reset does not re-open the breaker.
	recordCircuitBreakerFailure(name)
	require.False(t, isCircuitBreakerOpen(name))
}

// TestReconnectLoop_StopsWhenCircuitBreakerOpen verifies that reconnectLoop
// does not attempt a reconnect when the breaker is already open, and that
// it publishes StateCircuitOpen so the UI can surface the paused state.
func TestReconnectLoop_StopsWhenCircuitBreakerOpen(t *testing.T) {
	const name = "cb-loop-stops-test"

	circuitBreakers.Set(name, circuitBreakerState{
		failures:    circuitBreakerThreshold,
		windowStart: time.Now(),
		open:        true,
	})
	t.Cleanup(func() {
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	var callCount int32
	origFn := reconnectFn
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&callCount, 1)
		return errors.New("should not be called")
	}
	t.Cleanup(func() { reconnectFn = origFn })

	// reconnectFn is overridden to ignore the config store, so nil is safe
	// here. Avoiding loadTestStore prevents lumberjack log goroutines from
	// leaking into other tests' goleak checks.
	reconnectLoop(context.Background(), nil, name)

	require.Equal(t, int32(0), atomic.LoadInt32(&callCount),
		"reconnectFn must not be called when the breaker is open")

	info, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateCircuitOpen, info.State)
}

// TestReconnectLoop_AllFailuresOpenBreaker verifies that reconnectLoop
// retries up to len(reconnectBackoffs) times, records a failure per
// attempt, and trips the breaker once the threshold is reached.
func TestReconnectLoop_AllFailuresOpenBreaker(t *testing.T) {
	const name = "cb-loop-all-fail-test"

	origFn := reconnectFn
	origBackoffs := reconnectBackoffs
	var callCount int32
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&callCount, 1)
		return errors.New("simulated reconnect failure")
	}
	// Shorten backoffs so the test runs quickly.
	reconnectBackoffs = []time.Duration{
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
	}
	t.Cleanup(func() {
		reconnectFn = origFn
		reconnectBackoffs = origBackoffs
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	reconnectLoop(context.Background(), nil, name)

	// The loop should have attempted one reconnect per backoff slot.
	require.Equal(t, int32(len(origBackoffs)), atomic.LoadInt32(&callCount))

	// All attempts failed: the breaker should be open.
	require.True(t, isCircuitBreakerOpen(name))

	// The final state should be StateCircuitOpen.
	info, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateCircuitOpen, info.State)
}

// TestReconnectLoop_SucceedsAndResetsBreaker verifies that a successful
// reconnect resets the breaker and exits the loop early.
func TestReconnectLoop_SucceedsAndResetsBreaker(t *testing.T) {
	const name = "cb-loop-success-test"

	// Seed a couple of prior failures so we can verify they are cleared.
	recordCircuitBreakerFailure(name)
	recordCircuitBreakerFailure(name)
	require.False(t, isCircuitBreakerOpen(name))

	origFn := reconnectFn
	origBackoffs := reconnectBackoffs
	var callCount int32
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&callCount, 1)
		return nil // success
	}
	reconnectBackoffs = []time.Duration{time.Millisecond}
	t.Cleanup(func() {
		reconnectFn = origFn
		reconnectBackoffs = origBackoffs
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	reconnectLoop(context.Background(), nil, name)

	require.Equal(t, int32(1), atomic.LoadInt32(&callCount),
		"loop should exit after a successful reconnect")

	// A successful reconnect clears the breaker.
	_, ok := circuitBreakers.Get(name)
	require.False(t, ok, "breaker state should be cleared after success")
}

// TestReconnectLoop_StopsWhenDisconnecting verifies that reconnectLoop
// stops without calling reconnectFn when the server is marked as
// deliberately disconnecting.
func TestReconnectLoop_StopsWhenDisconnecting(t *testing.T) {
	const name = "cb-loop-disconnecting-test"

	markDisconnecting(name)
	t.Cleanup(func() {
		clearDisconnecting(name)
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	var callCount int32
	origFn := reconnectFn
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&callCount, 1)
		return errors.New("should not be called")
	}
	t.Cleanup(func() { reconnectFn = origFn })

	reconnectLoop(context.Background(), nil, name)

	require.Equal(t, int32(0), atomic.LoadInt32(&callCount),
		"reconnectFn must not be called while disconnecting")
}

// TestReconnectLoop_RespectsContextCancellation verifies that the loop
// exits promptly when its context is cancelled, without spawning further
// reconnect attempts.
func TestReconnectLoop_RespectsContextCancellation(t *testing.T) {
	const name = "cb-loop-cancel-test"

	ctx, cancel := context.WithCancel(context.Background())

	origFn := reconnectFn
	origBackoffs := reconnectBackoffs
	callCount := int32(0)
	// The first call cancels the context and returns an error; the loop
	// should then observe ctx.Done() and exit before the next attempt.
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&callCount, 1)
		cancel()
		return errors.New("simulated failure")
	}
	// Use a long backoff so the second attempt would be delayed; the
	// context cancellation must preempt it.
	reconnectBackoffs = []time.Duration{
		time.Second,
		time.Second,
	}
	t.Cleanup(func() {
		reconnectFn = origFn
		reconnectBackoffs = origBackoffs
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	reconnectLoop(ctx, nil, name)

	require.Equal(t, int32(1), atomic.LoadInt32(&callCount),
		"loop should stop after the context is cancelled")
}

// TestResetCircuitBreaker_PublicAPI verifies that the public
// ResetCircuitBreaker function clears the breaker, performs a manual
// reconnect, and allows a subsequent reconnectLoop to proceed.
func TestResetCircuitBreaker_PublicAPI(t *testing.T) {
	const name = "cb-reset-public-test"

	// Trip the breaker by recording threshold failures.
	for i := 0; i < circuitBreakerThreshold; i++ {
		recordCircuitBreakerFailure(name)
	}
	require.True(t, isCircuitBreakerOpen(name))

	origFn := reconnectFn
	var resetCallCount int32
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&resetCallCount, 1)
		return nil
	}
	t.Cleanup(func() {
		reconnectFn = origFn
		circuitBreakers.Del(name)
		states.Del(name)
		sessions.Del(name)
	})

	err := ResetCircuitBreaker(context.Background(), nil, name)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&resetCallCount))

	// The breaker must be cleared so auto-reconnect can resume.
	require.False(t, isCircuitBreakerOpen(name))

	// The disconnecting flag must be cleared after ResetCircuitBreaker
	// returns so future auto-reconnect loops are not blocked.
	require.False(t, isDisconnecting(name))

	// A subsequent reconnectLoop should be able to attempt a reconnect.
	var loopCallCount int32
	reconnectFn = func(context.Context, *config.ConfigStore, string) error {
		atomic.AddInt32(&loopCallCount, 1)
		return nil
	}
	reconnectLoop(context.Background(), nil, name)
	require.Equal(t, int32(1), atomic.LoadInt32(&loopCallCount),
		"reconnectLoop should proceed after ResetCircuitBreaker")
}

// TestTryStartReconnect_NoDuplicateLoops verifies that tryStartReconnect
// allows at most one concurrent reconnect loop per server. This is the
// dedup mechanism that prevents goroutine storms when multiple tool calls
// fail concurrently.
func TestTryStartReconnect_NoDuplicateLoops(t *testing.T) {
	const name = "cb-try-start-test"
	t.Cleanup(func() {
		stopReconnect(name)
		disconnecting.Del(name)
	})

	// First call claims the slot.
	require.True(t, tryStartReconnect(name))

	// A second call while the first is "running" must be rejected.
	require.False(t, tryStartReconnect(name))

	// After the loop exits, the slot is available again.
	stopReconnect(name)
	require.True(t, tryStartReconnect(name))
	stopReconnect(name)
}

// TestTryStartReconnect_RespectsDisconnecting verifies that
// tryStartReconnect refuses to start a loop for a server that is being
// deliberately disconnected.
func TestTryStartReconnect_RespectsDisconnecting(t *testing.T) {
	const name = "cb-try-start-disconnecting-test"
	markDisconnecting(name)
	t.Cleanup(func() {
		clearDisconnecting(name)
		stopReconnect(name)
	})

	require.False(t, tryStartReconnect(name),
		"must not start a loop while the server is disconnecting")
}

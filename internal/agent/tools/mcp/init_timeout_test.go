package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWaitForInitWithTimeout_AlreadyDone verifies that when initialization has
// already completed (initDone closed), the function returns nil immediately
// without waiting for the timeout. This models the fast-server scenario where
// all servers connect within the grace period.
func TestWaitForInitWithTimeout_AlreadyDone(t *testing.T) {
	t.Parallel()

	initDone := make(chan struct{})
	close(initDone)

	start := time.Now()
	err := waitForInitWithTimeout(initDone, 5*time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	// Should return near-instantly rather than blocking on the timeout.
	require.Less(t, elapsed, 250*time.Millisecond)
}

// TestWaitForInitWithTimeout_TimesOut verifies that when initialization does
// not complete within the timeout (slow server still connecting), the function
// returns an error after the timeout elapses. This models a slow MCP server
// whose connection exceeds the startup grace period.
func TestWaitForInitWithTimeout_TimesOut(t *testing.T) {
	t.Parallel()

	// A channel that is never closed models a slow server that hasn't finished
	// connecting yet.
	initDone := make(chan struct{})

	start := time.Now()
	err := waitForInitWithTimeout(initDone, 50*time.Millisecond)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "50ms")
	// Should return shortly after the timeout, not immediately.
	require.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	require.Less(t, elapsed, 500*time.Millisecond)
}

// TestWaitForInitWithTimeout_CompletesBeforeTimeout verifies that when
// initialization completes before the timeout (servers connect within the
// grace period), the function returns nil.
func TestWaitForInitWithTimeout_CompletesBeforeTimeout(t *testing.T) {
	t.Parallel()

	initDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Simulate a quick connection that completes well within the timeout.
		close(initDone)
	}()

	err := waitForInitWithTimeout(initDone, 1*time.Second)
	require.NoError(t, err)
	wg.Wait() // Ensure the goroutine exits before the test ends.
}

// TestWaitForInitWithTimeout_BackgroundContinues verifies the core "startup
// timeout does not block" contract: even though WaitForInitWithTimeout returns
// an error because the grace period elapsed, the background work (the
// connecting goroutine) is NOT cancelled and eventually completes. This models
// a slow MCP server that finishes connecting after the grace period and then
// publishes a state change.
func TestWaitForInitWithTimeout_BackgroundContinues(t *testing.T) {
	t.Parallel()

	initDone := make(chan struct{})
	var backgroundDone bool
	var mu sync.Mutex

	// Simulate a slow server connection that takes longer than the grace
	// period. The grace period timeout must NOT cancel this work.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		backgroundDone = true
		mu.Unlock()
		close(initDone)
	}()

	// The grace period (20ms) elapses while the "server" is still connecting.
	err := waitForInitWithTimeout(initDone, 20*time.Millisecond)
	require.Error(t, err, "grace period should elapse before background finishes")

	// At this point the background work has not finished yet.
	mu.Lock()
	require.False(t, backgroundDone, "background should still be running after timeout")
	mu.Unlock()

	// Wait for the background work to complete — it must NOT have been
	// cancelled by the timeout.
	wg.Wait()
	mu.Lock()
	require.True(t, backgroundDone, "background work should complete after the grace period")
	mu.Unlock()
}

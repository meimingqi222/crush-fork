package idempotency

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExecuteReplaysConcurrentOutcomeAndRejectsConflict(t *testing.T) {
	t.Parallel()

	store := New(Config{})
	t.Cleanup(store.Close)
	requestID := uuid.NewString()
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func() Outcome {
		calls.Add(1)
		<-release
		return Outcome{Value: map[string]string{"turnId": "turn-1"}}
	}

	results := make(chan Outcome, 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			outcome, err := store.Execute(context.Background(), "turn/start", requestID, map[string]string{"prompt": "same"}, fn)
			require.NoError(t, err)
			results <- outcome
		})
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	wait.Wait()
	close(results)
	for outcome := range results {
		require.Equal(t, map[string]string{"turnId": "turn-1"}, outcome.Value)
	}
	require.Equal(t, int32(1), calls.Load())

	_, err := store.Execute(t.Context(), "turn/start", requestID, map[string]string{"prompt": "different"}, func() Outcome { return Outcome{} })
	require.ErrorIs(t, err, ErrConflict)
}

func TestStoreBoundsTTLAndInflightCapacity(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	store := New(Config{TTL: time.Minute, MaxEntries: 1, Clock: func() time.Time { return now }})
	t.Cleanup(store.Close)
	_, err := store.Execute(t.Context(), "scope", uuid.NewString(), "first", func() Outcome { return Outcome{Value: "first"} })
	require.NoError(t, err)
	now = now.Add(2 * time.Minute)
	_, err = store.Execute(t.Context(), "scope", uuid.NewString(), "second", func() Outcome { return Outcome{Value: "second"} })
	require.NoError(t, err)

	blocked := New(Config{MaxEntries: 1})
	t.Cleanup(blocked.Close)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = blocked.Execute(context.Background(), "scope", uuid.NewString(), "inflight", func() Outcome {
			<-release
			return Outcome{}
		})
		close(done)
	}()
	require.Eventually(t, func() bool {
		blocked.mu.Lock()
		defer blocked.mu.Unlock()
		return len(blocked.entries) == 1
	}, time.Second, time.Millisecond)
	_, err = blocked.Execute(t.Context(), "scope", uuid.NewString(), "other", func() Outcome { return Outcome{} })
	require.ErrorIs(t, err, ErrCapacity)
	close(release)
	<-done
}

func TestExecuteValidatesUUIDAndCloseUnblocksDuplicates(t *testing.T) {
	t.Parallel()

	store := New(Config{})
	_, err := store.Execute(t.Context(), "scope", "not-a-uuid", nil, func() Outcome { return Outcome{} })
	require.Error(t, err)

	requestID := uuid.NewString()
	release := make(chan struct{})
	go func() {
		_, _ = store.Execute(context.Background(), "scope", requestID, nil, func() Outcome {
			<-release
			return Outcome{}
		})
	}()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.entries) == 1
	}, time.Second, time.Millisecond)
	type executionResult struct {
		outcome Outcome
		err     error
	}
	duplicate := make(chan executionResult, 1)
	go func() {
		outcome, err := store.Execute(context.Background(), "scope", requestID, nil, func() Outcome { return Outcome{} })
		duplicate <- executionResult{outcome: outcome, err: err}
	}()
	store.Close()
	result := <-duplicate
	if result.err != nil {
		require.ErrorIs(t, result.err, ErrClosed)
	} else {
		require.ErrorIs(t, result.outcome.Failure.(error), ErrClosed)
	}
	close(release)
}

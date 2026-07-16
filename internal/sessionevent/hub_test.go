package sessionevent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHubAllocatesStrictPerSessionSequencesConcurrently(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	defer hub.Close()
	const count = 500
	sequences := make([]uint64, count)
	var wg sync.WaitGroup
	for index := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			event, err := hub.Publish("session-a", NewEvent{Kind: KindSessionUpdated})
			require.NoError(t, err)
			sequences[index] = event.Sequence
		}()
	}
	wg.Wait()
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	for index, sequence := range sequences {
		require.Equal(t, uint64(index+1), sequence)
	}

	other, err := hub.Publish("session-b", NewEvent{Kind: KindSessionUpdated})
	require.NoError(t, err)
	require.Equal(t, uint64(1), other.Sequence)
}

func TestMCPStatusAdvancesSessionRevision(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	t.Cleanup(hub.Close)
	first, err := hub.Publish("session", NewEvent{
		Kind: KindMCPStatus, AdvanceRevision: true, Delivery: DeliveryReliable,
		Payload: MCPStatus{ServerID: "session:docs", Status: "starting", Revision: 1},
	})
	require.NoError(t, err)
	second, err := hub.Publish("session", NewEvent{
		Kind: KindMCPStatus, AdvanceRevision: true, Delivery: DeliveryReliable,
		Payload: MCPStatus{ServerID: "session:docs", Status: "connected", Revision: 2},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1), first.SessionRevision)
	require.Equal(t, uint64(2), second.SessionRevision)
	require.Equal(t, uint64(2), hub.LatestRevision("session"))
}

func TestHubSubscriptionPreservesConcurrentPublishOrder(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{QueueEvents: 256})
	defer hub.Close()
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)
	defer subscription.Close()

	const count = 100
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, publishErr := hub.Publish("session-a", NewEvent{Kind: KindToolCompleted, Delivery: DeliveryReliable})
			require.NoError(t, publishErr)
		}()
	}
	wg.Wait()

	for sequence := uint64(1); sequence <= count; sequence++ {
		event, nextErr := subscription.Next(t.Context())
		require.NoError(t, nextErr)
		require.Equal(t, sequence, event.Sequence)
	}
}

func TestHubCoalescesAdjacentTextDeltas(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{QueueEvents: 2})
	defer hub.Close()
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)
	defer subscription.Close()

	for _, text := range []string{"one", " two", " three"} {
		_, err = hub.Publish("session-a", NewEvent{
			Kind:        KindMessageDelta,
			Delivery:    DeliveryMerge,
			CoalesceKey: "message:part",
			Payload:     TextDelta{MessageID: "message", PartID: "part", Text: text},
		})
		require.NoError(t, err)
	}

	event, err := subscription.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(1), event.FirstSequence)
	require.Equal(t, uint64(3), event.Sequence)
	require.Equal(t, "one two three", event.Payload.(TextDelta).Text)
}

func TestHubOverflowPreservesReliableEventsAndRequiresSnapshot(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{QueueEvents: 2})
	defer hub.Close()
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)
	defer subscription.Close()

	for sequence := 1; sequence <= 3; sequence++ {
		_, err = hub.Publish("session-a", NewEvent{
			Kind:     KindPermissionRequested,
			Delivery: DeliveryReliable,
			Payload:  fmt.Sprintf("permission-%d", sequence),
		})
		require.NoError(t, err)
	}

	for sequence := uint64(1); sequence <= 3; sequence++ {
		event, nextErr := subscription.Next(t.Context())
		require.NoError(t, nextErr)
		require.Equal(t, sequence, event.Sequence)
		require.Equal(t, KindPermissionRequested, event.Kind)
	}
	marker, err := subscription.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, KindSnapshotRequired, marker.Kind)
	_, err = subscription.Next(t.Context())
	require.ErrorIs(t, err, ErrSnapshotRequired)
}

func TestHubIncompatibleRecoverableOverflowRequiresSnapshot(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{QueueEvents: 1})
	defer hub.Close()
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)
	defer subscription.Close()

	_, err = hub.Publish("session-a", NewEvent{Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "first", Payload: TextDelta{Text: "first"}})
	require.NoError(t, err)
	_, err = hub.Publish("session-a", NewEvent{Kind: KindReasoningDelta, Delivery: DeliveryMerge, CoalesceKey: "second", Payload: TextDelta{Text: "second"}})
	require.NoError(t, err)

	marker, err := subscription.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, KindSnapshotRequired, marker.Kind)
	_, err = subscription.Next(t.Context())
	require.ErrorIs(t, err, ErrSnapshotRequired)
}

func TestHubCopiesTerminalPayloadBeforeJournaling(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	defer hub.Close()
	data := []byte("safe")
	_, err := hub.Publish("session-a", NewEvent{
		Kind:        KindTerminalOutput,
		Delivery:    DeliveryMerge,
		CoalesceKey: "terminal",
		Payload:     TerminalOutput{TerminalID: "terminal", Data: data},
	})
	require.NoError(t, err)
	data[0] = 'X'

	events, err := hub.ReplayAfter("session-a", 0)
	require.NoError(t, err)
	require.Equal(t, []byte("safe"), events[0].Payload.(TerminalOutput).Data)
}

func TestHubReplayCountBound(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{JournalEvents: 3, JournalAge: time.Hour})
	defer hub.Close()
	for range 5 {
		_, err := hub.Publish("session-a", NewEvent{Kind: KindSessionUpdated})
		require.NoError(t, err)
	}

	events, err := hub.ReplayAfter("session-a", 2)
	require.NoError(t, err)
	require.Equal(t, []uint64{3, 4, 5}, eventSequences(events))
	_, err = hub.ReplayAfter("session-a", 1)
	require.ErrorIs(t, err, ErrSequenceExpired)
}

func TestHubCloseSessionUnblocksAndDetachesSubscription(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)

	hub.CloseSession("session-a")
	_, err = subscription.Next(context.Background())
	require.ErrorIs(t, err, ErrSubscriptionClosed)
	subscription.Close()

	_, err = hub.Publish("session-a", NewEvent{Kind: KindSessionUpdated})
	require.NoError(t, err, "closing one session must not close the hub")
	hub.Close()
	_, err = hub.Publish("session-b", NewEvent{Kind: KindSessionUpdated})
	require.True(t, errors.Is(err, ErrSubscriptionClosed))
}

func TestSubscriptionCloseUnblocksWaitingNext(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	defer hub.Close()
	subscription, err := hub.Subscribe("session-a", 0)
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		_, nextErr := subscription.Next(context.Background())
		result <- nextErr
	}()
	subscription.Close()

	select {
	case err = <-result:
		require.ErrorIs(t, err, ErrSubscriptionClosed)
	case <-time.After(time.Second):
		t.Fatal("waiting subscription did not unblock")
	}
}

func TestMetricKindBoundsUnknownValues(t *testing.T) {
	t.Parallel()

	require.Equal(t, "message", metricKind(KindMessageDelta))
	require.Equal(t, "other", metricKind(Kind("session-id-controlled-value")))
}

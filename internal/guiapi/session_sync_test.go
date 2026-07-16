package guiapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestSessionSyncReplaysOrderedWireEvents(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	first, err := hub.Publish("session-1", sessionevent.NewEvent{
		Kind:    sessionevent.KindMessageDelta,
		Payload: sessionevent.TextDelta{MessageID: "message-1", PartID: "part-1", Text: "hello"},
	})
	require.NoError(t, err)
	second, err := hub.Publish("session-1", sessionevent.NewEvent{
		Kind:    sessionevent.KindMessageCompleted,
		Payload: sessionevent.MessageEvent{MessageID: "message-1", FinishReason: "stop"},
	})
	require.NoError(t, err)

	service := negotiatedSessionSyncService(t, hub, &recordingWriter{})
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/sync", mustRawJSON(t, syncParams{
		SessionID:     "session-1",
		AfterSequence: 0,
	}))
	require.Nil(t, rpcErr)
	sync := result.(syncResult)
	require.Equal(t, "replay", sync.Mode)
	require.Equal(t, second.Sequence, sync.LatestSequence)
	require.Equal(t, []uint64{first.Sequence, second.Sequence}, []uint64{sync.Events[0].Sequence, sync.Events[1].Sequence})

	raw, err := json.Marshal(sync.Events)
	require.NoError(t, err)
	require.JSONEq(t, `[
		{"sessionId":"session-1","firstSequence":1,"sequence":1,"sessionRevision":0,"eventId":"`+first.EventID+`","timestamp":"`+first.Timestamp.UTC().Format(time.RFC3339Nano)+`","kind":"message.delta","payload":{"messageId":"message-1","partId":"part-1","text":"hello"}},
		{"sessionId":"session-1","firstSequence":2,"sequence":2,"sessionRevision":0,"eventId":"`+second.EventID+`","timestamp":"`+second.Timestamp.UTC().Format(time.RFC3339Nano)+`","kind":"message.completed","payload":{"messageId":"message-1","finishReason":"stop"}}
	]`, string(raw))
	require.NotContains(t, string(raw), "MergedCount")
	require.NotContains(t, string(raw), "Delivery")
	require.NotContains(t, string(raw), "CoalesceKey")
}

func TestSessionSyncReconnectAndExpiredSequence(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{JournalEvents: 2})
	for range 3 {
		_, err := hub.Publish("session-1", sessionevent.NewEvent{Kind: sessionevent.KindSessionUpdated})
		require.NoError(t, err)
	}
	service := negotiatedSessionSyncService(t, hub, &recordingWriter{})

	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/sync", mustRawJSON(t, syncParams{
		SessionID:     "session-1",
		AfterSequence: 2,
	}))
	require.Nil(t, rpcErr)
	require.Equal(t, []uint64{3}, []uint64{result.(syncResult).Events[0].Sequence})

	_, rpcErr = service.HandleExtension(t.Context(), "crush/session/sync", mustRawJSON(t, syncParams{
		SessionID:     "session-1",
		AfterSequence: 0,
	}))
	require.Equal(t, -32020, rpcErr.Code)
	require.Equal(t, errorSequenceExpired, rpcErr.Message)
	data := rpcErr.Data.(ErrorData)
	require.Equal(t, errorSequenceExpired, data.Code)
	require.True(t, data.Retryable)
	require.Equal(t, true, data.Details["snapshotRequired"])
	require.Equal(t, uint64(3), data.Details["latestSequence"])
}

func TestSubscribeLifecycleAndUnsubscribeAreIdempotent(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	writer := &recordingWriter{}
	service := negotiatedSessionSyncService(t, hub, writer)
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/subscribe", mustRawJSON(t, subscribeParams{SessionID: "session-1"}))
	require.Nil(t, rpcErr)
	lifecycle := result.(acp.ResponseLifecycle)
	subscriptionID := lifecycle.ResponseResult().(subscribeResult).SubscriptionID

	_, err := hub.Publish("session-1", sessionevent.NewEvent{Kind: sessionevent.KindTurnStarted})
	require.NoError(t, err)
	require.Empty(t, writer.snapshot())
	lifecycle.AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool { return len(writer.snapshot()) == 1 }, time.Second, time.Millisecond)
	lifecycle.AfterResponse(t.Context(), nil)

	for index, expected := range []bool{true, false} {
		result, rpcErr = service.HandleExtension(t.Context(), "crush/session/unsubscribe", mustRawJSON(t, unsubscribeParams{SubscriptionID: subscriptionID}))
		require.Nil(t, rpcErr)
		require.Equal(t, expected, result.(unsubscribeResult).Unsubscribed, "attempt %d", index+1)
	}
	service.Close()
	service.Close()
}

func TestSubscribeWriteFailureCleansUpWithoutStartingWriter(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	writer := &recordingWriter{}
	service := negotiatedSessionSyncService(t, hub, writer)
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/subscribe", mustRawJSON(t, subscribeParams{SessionID: "session-1"}))
	require.Nil(t, rpcErr)
	lifecycle := result.(acp.ResponseLifecycle)
	subscriptionID := lifecycle.ResponseResult().(subscribeResult).SubscriptionID
	lifecycle.AfterResponse(t.Context(), errors.New("broken response writer"))
	lifecycle.AfterResponse(t.Context(), nil)

	_, err := hub.Publish("session-1", sessionevent.NewEvent{Kind: sessionevent.KindTurnStarted})
	require.NoError(t, err)
	require.Never(t, func() bool { return len(writer.snapshot()) > 0 }, 20*time.Millisecond, time.Millisecond)
	result, rpcErr = service.HandleExtension(t.Context(), "crush/session/unsubscribe", mustRawJSON(t, unsubscribeParams{SubscriptionID: subscriptionID}))
	require.Nil(t, rpcErr)
	require.False(t, result.(unsubscribeResult).Unsubscribed)
}

func TestBlockedNotificationWriterDoesNotBackpressurePublishAndPreservesReliableEvents(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{QueueEvents: 2})
	writer := &recordingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	service := negotiatedSessionSyncService(t, hub, writer)
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/subscribe", mustRawJSON(t, subscribeParams{SessionID: "session-1"}))
	require.Nil(t, rpcErr)
	result.(acp.ResponseLifecycle).AfterResponse(t.Context(), nil)

	_, err := hub.Publish("session-1", reliableTurnEvent())
	require.NoError(t, err)
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("notification writer was not entered")
	}

	started := time.Now()
	for range 3 {
		_, err = hub.Publish("session-1", reliableTurnEvent())
		require.NoError(t, err)
	}
	require.Less(t, time.Since(started), 100*time.Millisecond)
	close(writer.release)

	require.Eventually(t, func() bool { return len(writer.snapshot()) == 5 }, time.Second, time.Millisecond)
	notifications := writer.snapshot()
	sequences := make([]uint64, 0, len(notifications))
	for _, notification := range notifications {
		sequences = append(sequences, notification.Event.Sequence)
	}
	require.Equal(t, []uint64{1, 2, 3, 4, 4}, sequences)
	require.Equal(t, sessionevent.KindSnapshotRequired, notifications[len(notifications)-1].Event.Kind)
}

func TestConcurrentPublishUnsubscribeAndClose(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	service := negotiatedSessionSyncService(t, hub, &recordingWriter{})
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/subscribe", mustRawJSON(t, subscribeParams{SessionID: "session-1"}))
	require.Nil(t, rpcErr)
	lifecycle := result.(acp.ResponseLifecycle)
	subscriptionID := lifecycle.ResponseResult().(subscribeResult).SubscriptionID
	lifecycle.AfterResponse(t.Context(), nil)

	var wait sync.WaitGroup
	wait.Go(func() {
		for range 100 {
			_, _ = hub.Publish("session-1", reliableTurnEvent())
		}
	})
	wait.Go(func() {
		_, _ = service.HandleExtension(context.Background(), "crush/session/unsubscribe", mustRawJSON(t, unsubscribeParams{SubscriptionID: subscriptionID}))
	})
	wait.Go(service.Close)
	wait.Wait()
}

type recordingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	events  []eventNotification
}

func (w *recordingWriter) NotifySync(ctx context.Context, method string, params any) error {
	if method != sessionEventNotification {
		return errors.New("unexpected notification method")
	}
	w.once.Do(func() {
		if w.entered != nil {
			close(w.entered)
		}
	})
	if w.release != nil {
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.mu.Lock()
	w.events = append(w.events, params.(eventNotification))
	w.mu.Unlock()
	return nil
}

func (w *recordingWriter) snapshot() []eventNotification {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]eventNotification(nil), w.events...)
}

func negotiatedSessionSyncService(t *testing.T, hub *sessionevent.Hub, writer NotificationWriter) *Service {
	t.Helper()
	service := NewService(hub)
	service.SetNotificationWriter(writer)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureSessionSync},
	})))
	t.Cleanup(service.Close)
	return service
}

func mustRawJSON(t testing.TB, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func reliableTurnEvent() sessionevent.NewEvent {
	return sessionevent.NewEvent{Kind: sessionevent.KindTurnStarted, Delivery: sessionevent.DeliveryReliable}
}

package guiapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/turn"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTurnStartIsImmediateIdempotentAndConflictSafe(t *testing.T) {
	t.Parallel()

	env := newTurnHandlerEnvironment(t)
	requestID := uuid.NewString()
	params := turnStartParams{
		SessionID:       env.sessionID,
		Content:         []turnContentBlock{{Type: "text", Text: "hello"}},
		ClientRequestID: requestID,
	}
	started := time.Now()
	first, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Less(t, time.Since(started), 20*time.Millisecond)
	firstTurn := first.(turn.Turn)
	require.Positive(t, firstTurn.AcceptedSequence)

	second, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Equal(t, firstTurn.ID, second.(turn.Turn).ID)
	require.Eventually(t, func() bool { return env.runner.calls.Load() == 1 }, time.Second, time.Millisecond)

	params.Content[0].Text = "different"
	_, rpcErr = env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, params))
	require.Equal(t, errorIdempotencyConflict, rpcErr.Message)
	require.Eventually(t, func() bool { return env.runner.calls.Load() == 1 }, time.Second, time.Millisecond)
}

func TestConcurrentDuplicateTurnStartExecutesOnce(t *testing.T) {
	t.Parallel()

	env := newTurnHandlerEnvironment(t)
	params := turnStartParams{
		SessionID:       env.sessionID,
		Content:         []turnContentBlock{{Type: "text", Text: "same"}},
		ClientRequestID: uuid.NewString(),
	}
	ids := make(chan string, 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			result, rpcErr := env.service.HandleExtension(context.Background(), "crush/turn/start", mustRawJSON(t, params))
			require.Nil(t, rpcErr)
			ids <- result.(turn.Turn).ID
		})
	}
	wait.Wait()
	close(ids)
	var expected string
	for id := range ids {
		if expected == "" {
			expected = id
		}
		require.Equal(t, expected, id)
	}
	require.Eventually(t, func() bool { return env.runner.calls.Load() == 1 }, time.Second, time.Millisecond)
}

func TestTurnStartCarriesValidatedInferenceOverrides(t *testing.T) {
	t.Parallel()

	env := newTurnHandlerEnvironment(t)
	temperature := 0.8
	result, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, turnStartParams{
		SessionID: env.sessionID, Content: []turnContentBlock{{Type: "text", Text: "override"}},
		Inference: session.InferenceOverrides{Temperature: &temperature}, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	value := result.(turn.Turn)
	require.Eventually(t, func() bool {
		inference, ok := env.runner.inferenceFor(value.ID)
		return ok && inference.Temperature != nil && *inference.Temperature == 0.8
	}, time.Second, time.Millisecond)
}

func TestTurnInputRejectsInlineBinaryOverMandatoryBlobThreshold(t *testing.T) {
	encodedLength := base64.StdEncoding.EncodedLen(maxInlineBinaryBytes + 1)
	service := NewService(nil)
	t.Cleanup(service.Close)
	_, rpcErr := service.turnInput(t.Context(), "session-1", []turnContentBlock{{
		Type: "image", Data: strings.Repeat("A", encodedLength),
	}}, session.InferenceOverrides{})
	require.Equal(t, errorPayloadTooLarge, rpcErr.Message)
}

func BenchmarkTurnStartAcknowledgement(b *testing.B) {
	env := newTurnHandlerEnvironment(b)
	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		params := turnStartParams{
			SessionID: env.sessionID,
			Content: []turnContentBlock{{
				Type: "text",
				Text: "benchmark prompt",
			}},
			ClientRequestID: uuid.NewString(),
		}
		started := b.Elapsed()
		result, rpcErr := env.service.HandleExtension(context.Background(), "crush/turn/start", mustRawJSON(b, params))
		durations = append(durations, b.Elapsed()-started)
		if rpcErr != nil {
			b.Fatal(rpcErr)
		}
		b.StopTimer()
		value := result.(turn.Turn)
		env.runner.complete(value.ID, turn.StatusCompleted)
		if _, err := env.turns.Wait(context.Background(), value.ID); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if len(durations) > 0 {
		index := (len(durations)*95 + 99) / 100
		b.ReportMetric(float64(durations[index-1])/float64(time.Microsecond), "p95-us")
	}
}

func TestTurnCancellationAcknowledgementP95BelowHundredMilliseconds(t *testing.T) {
	t.Parallel()

	env := newTurnHandlerEnvironment(t)
	durations := make([]time.Duration, 100)
	for index := range durations {
		current := startHandlerTurn(t, env, "cancel p95")
		require.Eventually(t, func() bool {
			value, err := env.turns.Get(current.ID)
			return err == nil && value.Status == turn.StatusRunning
		}, time.Second, time.Millisecond)
		started := time.Now()
		result, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/cancel", mustRawJSON(t, turnCancelParams{
			SessionID: env.sessionID, TurnID: current.ID, ClientRequestID: uuid.NewString(),
		}))
		durations[index] = time.Since(started)
		require.Nil(t, rpcErr)
		require.True(t, result.(cancelResult).Acknowledged)
		terminalTurn, err := env.turns.Wait(timeoutContextGUI(t), current.ID)
		require.NoError(t, err)
		require.Equal(t, turn.StatusCancelled, terminalTurn.Status)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	require.Less(t, durations[94], 100*time.Millisecond)
}

func TestTurnQueueHandlersCancelRetryAndSteer(t *testing.T) {
	t.Parallel()

	env := newTurnHandlerEnvironment(t)
	first := startHandlerTurn(t, env, "first")
	require.Eventually(t, func() bool {
		value, err := env.turns.Get(first.ID)
		return err == nil && value.Status == turn.StatusRunning
	}, time.Second, time.Millisecond)
	second := startHandlerTurn(t, env, "second")
	third := startHandlerTurn(t, env, "third")

	queueResult, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/queue/list", mustRawJSON(t, queueListParams{SessionID: env.sessionID}))
	require.Nil(t, rpcErr)
	queue := queueResult.(turn.Queue)
	require.Equal(t, []string{second.ID, third.ID}, queuedIDs(queue))

	reorderedResult, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/queue/reorder", mustRawJSON(t, queueReorderParams{
		SessionID: env.sessionID, ExpectedRevision: queue.Revision, TurnIDs: []string{third.ID}, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	reordered := reorderedResult.(turn.Queue)
	require.Equal(t, []string{third.ID, second.ID}, queuedIDs(reordered))

	removeID := uuid.NewString()
	removedResult, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/queue/remove", mustRawJSON(t, queueRemoveParams{
		SessionID: env.sessionID, TurnID: second.ID, ClientRequestID: removeID,
	}))
	require.Nil(t, rpcErr)
	removedReplay, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/queue/remove", mustRawJSON(t, queueRemoveParams{
		SessionID: env.sessionID, TurnID: second.ID, ClientRequestID: removeID,
	}))
	require.Nil(t, rpcErr)
	require.Equal(t, removedResult, removedReplay)

	cancelStarted := time.Now()
	cancelResultValue, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/cancel", mustRawJSON(t, turnCancelParams{
		SessionID: env.sessionID, TurnID: first.ID, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.Less(t, time.Since(cancelStarted), 100*time.Millisecond)
	require.True(t, cancelResultValue.(cancelResult).Acknowledged)
	terminal, err := env.turns.Wait(timeoutContextGUI(t), first.ID)
	require.NoError(t, err)
	require.Equal(t, turn.StatusCancelled, terminal.Status)

	retryResult, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/retry", mustRawJSON(t, retryParams{
		SessionID: env.sessionID, TurnID: first.ID, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.NotEqual(t, first.ID, retryResult.(turn.Turn).ID)

	env.runner.steer.Store(true)
	steered, rpcErr := env.service.HandleExtension(t.Context(), "crush/session/steer", mustRawJSON(t, steerParams{
		SessionID: env.sessionID, Content: []turnContentBlock{{Type: "text", Text: "direction"}}, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.Equal(t, "steered", steered.(steerResult).Mode)
	require.Positive(t, steered.(steerResult).AcceptedSequence)
}

type turnHandlerEnvironment struct {
	service   *Service
	turns     *turn.Service
	runner    *handlerRunner
	sessionID string
}

func newTurnHandlerEnvironment(t testing.TB) *turnHandlerEnvironment {
	t.Helper()
	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := &handlerRunner{
		hub: hub, releases: make(map[string]chan turn.Status), active: make(map[string]string),
		inference: make(map[string]session.InferenceOverrides), attachments: make(map[string][]message.Attachment),
	}
	turns := turn.New(hub, runner, nil, turn.Config{})
	store := idempotency.New(idempotency.Config{})
	service := NewService(hub)
	service.SetSessionContentSources(fixedSessionReader{id: "session-1"}, nil, nil)
	service.SetTurnServices(turns, store)
	service.SetInferenceResolver(allowInferenceResolver{})
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureSessionControl},
	})))
	t.Cleanup(func() {
		service.Close()
		turns.Close()
		store.Close()
		hub.Close()
	})
	return &turnHandlerEnvironment{service: service, turns: turns, runner: runner, sessionID: "session-1"}
}

func startHandlerTurn(t *testing.T, env *turnHandlerEnvironment, prompt string) turn.Turn {
	t.Helper()
	result, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, turnStartParams{
		SessionID: env.sessionID, Content: []turnContentBlock{{Type: "text", Text: prompt}}, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	return result.(turn.Turn)
}

type fixedSessionReader struct{ id string }

func (r fixedSessionReader) Get(_ context.Context, id string) (session.Session, error) {
	if id != r.id {
		return session.Session{}, sql.ErrNoRows
	}
	return session.Session{ID: id}, nil
}

type handlerRunner struct {
	hub         *sessionevent.Hub
	calls       atomic.Int32
	steer       atomic.Bool
	mu          sync.Mutex
	releases    map[string]chan turn.Status
	active      map[string]string
	inference   map[string]session.InferenceOverrides
	attachments map[string][]message.Attachment
}

func (r *handlerRunner) Run(ctx context.Context, sessionID, _ string, attachments ...message.Attachment) error {
	r.calls.Add(1)
	turnID := agent.TurnIDFromContext(ctx)
	inference, _ := agent.TurnInferenceOverridesFromContext(ctx)
	release := make(chan turn.Status, 1)
	r.mu.Lock()
	r.releases[turnID] = release
	r.active[sessionID] = turnID
	r.inference[turnID] = inference
	r.attachments[turnID] = append([]message.Attachment(nil), attachments...)
	r.mu.Unlock()
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{Kind: sessionevent.KindTurnStarted, Delivery: sessionevent.DeliveryReliable, Payload: sessionevent.TurnEvent{TurnID: turnID}})
	status := <-release
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{Kind: guiTerminalKind(status), Delivery: sessionevent.DeliveryReliable, Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: string(status)}})
	return nil
}

func (r *handlerRunner) inferenceFor(turnID string) (session.InferenceOverrides, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.inference[turnID]
	return value, ok
}

func (r *handlerRunner) attachmentsFor(turnID string) []message.Attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]message.Attachment(nil), r.attachments[turnID]...)
}

func (r *handlerRunner) Cancel(sessionID string) {
	r.mu.Lock()
	turnID := r.active[sessionID]
	release := r.releases[turnID]
	r.mu.Unlock()
	if release == nil {
		return
	}
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{Kind: sessionevent.KindCancelAcknowledged, Delivery: sessionevent.DeliveryReliable, Payload: sessionevent.TurnEvent{TurnID: turnID}})
	select {
	case release <- turn.StatusCancelled:
	default:
	}
}

func (r *handlerRunner) complete(turnID string, status turn.Status) {
	for {
		r.mu.Lock()
		release := r.releases[turnID]
		r.mu.Unlock()
		if release != nil {
			release <- status
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *handlerRunner) RemoveQueuedTurn(string, string) bool { return false }
func (r *handlerRunner) Steer(string, string, ...message.Attachment) bool {
	return r.steer.Load()
}

func guiTerminalKind(status turn.Status) sessionevent.Kind {
	if status == turn.StatusCancelled {
		return sessionevent.KindTurnCancelled
	}
	if status == turn.StatusFailed {
		return sessionevent.KindTurnFailed
	}
	return sessionevent.KindTurnCompleted
}

type allowInferenceResolver struct{}

func (allowInferenceResolver) ValidateInferenceOverrides(context.Context, string, session.InferenceOverrides) error {
	return nil
}

func (allowInferenceResolver) EffectiveInference(context.Context, string) (session.EffectiveInference, error) {
	return session.EffectiveInference{}, nil
}

func (allowInferenceResolver) DefaultInference(context.Context) (session.EffectiveInference, error) {
	return session.EffectiveInference{}, nil
}

func queuedIDs(queue turn.Queue) []string {
	ids := make([]string, len(queue.Turns))
	for index, item := range queue.Turns {
		ids[index] = item.TurnID
	}
	return ids
}

func timeoutContextGUI(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

package turn

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestServicePreservesConnectionScopeThroughDispatchAndRetryClone(t *testing.T) {
	t.Parallel()
	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{})
	t.Cleanup(service.Close)

	input := Input{Prompt: "scoped", Scope: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, turnScopeTestKey{}, "connection-session-scope")
	}}
	started, err := service.Start(t.Context(), "session-1", input)
	require.NoError(t, err)
	require.Equal(t, started.ID, receiveTurnID(t, runner.started))
	require.Equal(t, "connection-session-scope", runner.scopeValue(started.ID))
	runner.complete(started.ID, StatusFailed)
	_, err = service.Wait(t.Context(), started.ID)
	require.NoError(t, err)

	retried, err := service.RetryTurn(t.Context(), started.ID)
	require.NoError(t, err)
	require.Equal(t, retried.ID, receiveTurnID(t, runner.started))
	require.Equal(t, "connection-session-scope", runner.scopeValue(retried.ID))
	runner.complete(retried.ID, StatusCompleted)
}

func TestServiceSerializesRunsAndReordersPendingQueue(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{})
	t.Cleanup(service.Close)

	first, err := service.Start(t.Context(), "session-1", Input{Prompt: "first"})
	require.NoError(t, err)
	require.Positive(t, first.AcceptedSequence)
	require.Equal(t, first.ID, receiveTurnID(t, runner.started))
	require.Eventually(t, func() bool {
		value, err := service.Get(first.ID)
		return err == nil && value.Status == StatusRunning
	}, time.Second, time.Millisecond)
	second, err := service.Start(t.Context(), "session-1", Input{Prompt: "second"})
	require.NoError(t, err)
	third, err := service.Start(t.Context(), "session-1", Input{Prompt: "third"})
	require.NoError(t, err)

	queue := service.Queue("session-1")
	require.Equal(t, []string{second.ID, third.ID}, queueTurnIDs(queue))
	reordered, err := service.Reorder("session-1", queue.Revision, []string{third.ID})
	require.NoError(t, err)
	require.Equal(t, []string{third.ID, second.ID}, queueTurnIDs(reordered))
	_, err = service.Reorder("session-1", queue.Revision, nil)
	require.ErrorIs(t, err, ErrRevisionConflict)

	runner.complete(first.ID, StatusCompleted)
	require.Equal(t, third.ID, receiveTurnID(t, runner.started))
	runner.complete(third.ID, StatusCompleted)
	require.Equal(t, second.ID, receiveTurnID(t, runner.started))
	runner.complete(second.ID, StatusCompleted)

	for _, turnID := range []string{first.ID, second.ID, third.ID} {
		completed, err := service.Wait(timeoutContext(t), turnID)
		require.NoError(t, err)
		require.Equal(t, StatusCompleted, completed.Status)
	}
	require.Equal(t, 1, runner.maxActive())
}

func TestWaitDisconnectDoesNotCancelAndCancellationMilestonesAreOrdered(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{})
	t.Cleanup(service.Close)

	active, err := service.Start(t.Context(), "session-1", Input{Prompt: "active"})
	require.NoError(t, err)
	require.Equal(t, active.ID, receiveTurnID(t, runner.started))
	require.Eventually(t, func() bool {
		value, err := service.Get(active.ID)
		return err == nil && value.Status == StatusRunning
	}, time.Second, time.Millisecond)
	queued, err := service.Start(t.Context(), "session-1", Input{Prompt: "queued"})
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	_, err = service.Wait(waitCtx, active.ID)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, StatusRunning, mustTurn(t, service, active.ID).Status)

	cancelledQueued, err := service.Cancel(queued.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, cancelledQueued.Status)
	cancelledActive, err := service.Cancel(active.ID)
	require.NoError(t, err)
	require.Contains(t, []Status{StatusCancelRequested, StatusCancelled}, cancelledActive.Status)
	terminal, err := service.Wait(timeoutContext(t), active.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, terminal.Status)

	events, err := hub.ReplayAfter("session-1", 0)
	require.NoError(t, err)
	ackSequence, terminalSequence := uint64(0), uint64(0)
	for _, event := range events {
		payload, ok := event.Payload.(sessionevent.TurnEvent)
		if !ok || payload.TurnID != active.ID {
			continue
		}
		if event.Kind == sessionevent.KindCancelAcknowledged && ackSequence == 0 {
			ackSequence = event.Sequence
		}
		if event.Kind == sessionevent.KindTurnCancelled {
			terminalSequence = event.Sequence
		}
	}
	require.Positive(t, ackSequence)
	require.Greater(t, terminalSequence, ackSequence)
}

func TestRetryAndSteer(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	messageTemperature := 0.6
	retry := &fakeRetrySource{input: Input{Prompt: "from message", Inference: session.InferenceOverrides{Temperature: &messageTemperature}}}
	service := New(hub, runner, retry, Config{})
	t.Cleanup(service.Close)

	originalTemperature := 0.9
	failed, err := service.Start(t.Context(), "session-1", Input{
		Prompt: "original", Inference: session.InferenceOverrides{Temperature: &originalTemperature},
	})
	require.NoError(t, err)
	require.Equal(t, failed.ID, receiveTurnID(t, runner.started))
	runner.complete(failed.ID, StatusFailed)
	failed, err = service.Wait(timeoutContext(t), failed.ID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, failed.Status)

	retried, err := service.RetryTurn(t.Context(), failed.ID)
	require.NoError(t, err)
	require.NotEqual(t, failed.ID, retried.ID)
	require.Equal(t, retried.ID, receiveTurnID(t, runner.started))
	require.Equal(t, "original", runner.prompt(retried.ID))
	require.Equal(t, 0.9, *runner.inference(retried.ID).Temperature)
	runner.complete(retried.ID, StatusCompleted)
	_, err = service.Wait(timeoutContext(t), retried.ID)
	require.NoError(t, err)

	messageRetry, err := service.RetryMessage(t.Context(), "session-1", "message-1")
	require.NoError(t, err)
	require.Equal(t, messageRetry.ID, receiveTurnID(t, runner.started))
	require.Equal(t, "from message", runner.prompt(messageRetry.ID))
	require.Equal(t, 0.6, *runner.inference(messageRetry.ID).Temperature)
	runner.complete(messageRetry.ID, StatusCompleted)

	runner.setSteer(true)
	mode, queued, sequence, err := service.Steer("session-1", Input{Prompt: "direction"})
	require.NoError(t, err)
	require.Equal(t, "steered", mode)
	require.Nil(t, queued)
	require.Positive(t, sequence)
}

func TestAttachmentResolutionFailureAdvancesQueue(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{})
	t.Cleanup(service.Close)

	active, err := service.Start(t.Context(), "session-1", Input{Prompt: "active"})
	require.NoError(t, err)
	require.Equal(t, active.ID, receiveTurnID(t, runner.started))
	failed, err := service.Start(t.Context(), "session-1", Input{
		AttachmentReferences: []AttachmentReference{{
			ID: "expired", Resolve: func(context.Context) (message.Attachment, error) {
				return message.Attachment{}, errors.New("expired")
			},
		}},
	})
	require.NoError(t, err)
	next, err := service.Start(t.Context(), "session-1", Input{Prompt: "next"})
	require.NoError(t, err)
	runner.complete(active.ID, StatusCompleted)
	failedResult, err := service.Wait(timeoutContext(t), failed.ID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, failedResult.Status)
	require.Equal(t, "attachment_unavailable", failedResult.Reason)
	require.Equal(t, next.ID, receiveTurnID(t, runner.started))
	runner.complete(next.ID, StatusCompleted)
}

func TestConcurrentStartsRemainBoundedAndRaceFree(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{MaxQueue: 32})
	t.Cleanup(service.Close)

	var wait sync.WaitGroup
	results := make(chan error, 64)
	for index := range 64 {
		wait.Go(func() {
			_, err := service.Start(context.Background(), "session-1", Input{Prompt: "prompt"})
			results <- err
		})
		_ = index
	}
	wait.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if errors.Is(err, ErrQueueFull) {
			rejected++
		} else {
			require.NoError(t, err)
			accepted++
		}
	}
	require.Equal(t, 32, accepted)
	require.Equal(t, 32, rejected)
}

func TestStartEnforcesGlobalTurnAndRetainedInputBounds(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{MaxTurns: 2, MaxInputBytes: 8})
	t.Cleanup(service.Close)

	_, err := service.Start(t.Context(), "session-1", Input{Prompt: "123456789"})
	require.ErrorIs(t, err, ErrInputTooLarge)
	_, err = service.Start(t.Context(), "session-1", Input{Prompt: "first"})
	require.NoError(t, err)
	_, err = service.Start(t.Context(), "session-2", Input{Prompt: "second"})
	require.NoError(t, err)
	_, err = service.Start(t.Context(), "session-3", Input{Prompt: "third"})
	require.ErrorIs(t, err, ErrQueueFull)
	require.Len(t, service.sessions, 2)
}

func TestStartAcknowledgementP95BelowTwentyMilliseconds(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{})
	runner := newFakeRunner(hub)
	service := New(hub, runner, nil, Config{MaxQueue: 128})
	t.Cleanup(service.Close)
	first, err := service.Start(t.Context(), "session-1", Input{Prompt: "active"})
	require.NoError(t, err)
	require.Equal(t, first.ID, receiveTurnID(t, runner.started))

	durations := make([]time.Duration, 100)
	for index := range durations {
		started := time.Now()
		_, err := service.Start(t.Context(), "session-1", Input{Prompt: "queued"})
		durations[index] = time.Since(started)
		require.NoError(t, err)
	}
	slices.Sort(durations)
	p95 := durations[94]
	require.Less(t, p95, 20*time.Millisecond)
}

type fakeRunner struct {
	hub *sessionevent.Hub

	mu          sync.Mutex
	releases    map[string]chan Status
	prompts     map[string]string
	inferences  map[string]session.InferenceOverrides
	scopeValues map[string]string
	activeBySes map[string]string
	active      int
	maximum     int
	steer       bool
	started     chan string
}

func newFakeRunner(hub *sessionevent.Hub) *fakeRunner {
	return &fakeRunner{
		hub: hub, releases: make(map[string]chan Status), prompts: make(map[string]string),
		inferences:  make(map[string]session.InferenceOverrides),
		scopeValues: make(map[string]string),
		activeBySes: make(map[string]string), started: make(chan string, 128),
	}
}

func (r *fakeRunner) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) error {
	turnID := agent.TurnIDFromContext(ctx)
	release := make(chan Status, 1)
	r.mu.Lock()
	r.releases[turnID] = release
	r.prompts[turnID] = prompt
	inference, _ := agent.TurnInferenceOverridesFromContext(ctx)
	r.inferences[turnID] = inference
	r.scopeValues[turnID], _ = ctx.Value(turnScopeTestKey{}).(string)
	r.activeBySes[sessionID] = turnID
	r.active++
	r.maximum = max(r.maximum, r.active)
	r.mu.Unlock()
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindTurnStarted, Delivery: sessionevent.DeliveryReliable,
		Payload: sessionevent.TurnEvent{TurnID: turnID},
	})
	r.started <- turnID
	var status Status
	select {
	case status = <-release:
	case <-ctx.Done():
		status = StatusCancelled
	}
	kind := terminalKind(status)
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{
		Kind: kind, Delivery: sessionevent.DeliveryReliable,
		Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: string(status)},
	})
	r.mu.Lock()
	delete(r.activeBySes, sessionID)
	r.active--
	r.mu.Unlock()
	return nil
}

func (r *fakeRunner) Cancel(sessionID string) {
	r.mu.Lock()
	turnID := r.activeBySes[sessionID]
	release := r.releases[turnID]
	r.mu.Unlock()
	if turnID == "" {
		return
	}
	_, _ = r.hub.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindCancelAcknowledged, Delivery: sessionevent.DeliveryReliable,
		Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: "user_requested"},
	})
	select {
	case release <- StatusCancelled:
	default:
	}
}

func (r *fakeRunner) RemoveQueuedTurn(string, string) bool { return false }

func (r *fakeRunner) Steer(string, string, ...message.Attachment) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steer
}

func (r *fakeRunner) complete(turnID string, status Status) {
	r.mu.Lock()
	release := r.releases[turnID]
	r.mu.Unlock()
	release <- status
}

func (r *fakeRunner) prompt(turnID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prompts[turnID]
}

func (r *fakeRunner) inference(turnID string) session.InferenceOverrides {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inferences[turnID]
}

type turnScopeTestKey struct{}

func (r *fakeRunner) scopeValue(turnID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scopeValues[turnID]
}

func (r *fakeRunner) maxActive() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximum
}

func (r *fakeRunner) setSteer(value bool) {
	r.mu.Lock()
	r.steer = value
	r.mu.Unlock()
}

type fakeRetrySource struct{ input Input }

func (s *fakeRetrySource) RetryInput(context.Context, string, string) (Input, error) {
	return s.input, nil
}

func receiveTurnID(t *testing.T, values <-chan string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn start")
		return ""
	}
}

func timeoutContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func queueTurnIDs(queue Queue) []string {
	ids := make([]string, len(queue.Turns))
	for index, item := range queue.Turns {
		ids[index] = item.TurnID
	}
	return ids
}

func mustTurn(t *testing.T, service *Service, turnID string) Turn {
	t.Helper()
	value, err := service.Get(turnID)
	require.NoError(t, err)
	return value
}

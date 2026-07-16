package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

type scriptedLiveAgent struct {
	failUpdates *atomic.Bool
	updateCount *atomic.Int32
	deltaCounts chan [2]int32
	ready       chan struct{}
	block       bool
}

func (*scriptedLiveAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("unexpected Generate call")
}

func (a *scriptedLiveAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		var err error
		ctx, _, err = call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
		if err != nil {
			return nil, err
		}
	}
	if a.ready != nil {
		close(a.ready)
	}
	if a.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if call.OnReasoningStart != nil {
		if err := call.OnReasoningStart("reasoning-1", fantasy.ReasoningContent{Text: "think"}); err != nil {
			return nil, err
		}
	}
	if call.OnReasoningDelta != nil {
		if err := call.OnReasoningDelta("reasoning-1", " more"); err != nil {
			return nil, err
		}
	}
	if call.OnReasoningEnd != nil {
		if err := call.OnReasoningEnd("reasoning-1", fantasy.ReasoningContent{}); err != nil {
			return nil, err
		}
	}
	if call.OnTextDelta != nil {
		before := int32(0)
		if a.updateCount != nil {
			before = a.updateCount.Load()
		}
		if err := call.OnTextDelta("text-1", "hello"); err != nil {
			return nil, err
		}
		if a.deltaCounts != nil {
			a.deltaCounts <- [2]int32{before, a.updateCount.Load()}
		}
	}
	if call.OnToolInputStart != nil {
		if err := call.OnToolInputStart("tool-1", "view"); err != nil {
			return nil, err
		}
	}
	if call.OnToolCall != nil {
		if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: "tool-1", ToolName: "view", Input: `{"path":"README.md"}`}); err != nil {
			return nil, err
		}
	}
	if call.OnToolResult != nil {
		if err := call.OnToolResult(fantasy.ToolResultContent{
			ToolCallID: "tool-1",
			ToolName:   "view",
			Result:     fantasy.ToolResultOutputContentText{Text: "file contents"},
		}); err != nil {
			return nil, err
		}
	}
	usage := fantasy.Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2}
	step := fantasy.StepResult{Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop, Usage: usage}}
	if a.failUpdates != nil {
		a.failUpdates.Store(true)
	}
	if call.OnStepFinish != nil {
		if err := call.OnStepFinish(step); err != nil {
			return nil, err
		}
	}
	return &fantasy.AgentResult{Steps: []fantasy.StepResult{step}, Response: step.Response, TotalUsage: usage}, nil
}

type failingUpdateMessageService struct {
	message.Service
	fail atomic.Bool
}

type countingUpdateMessageService struct {
	message.Service
	updates atomic.Int32
}

func (s *countingUpdateMessageService) Update(ctx context.Context, msg message.Message) error {
	s.updates.Add(1)
	return s.Service.Update(ctx, msg)
}

func (s *failingUpdateMessageService) Update(ctx context.Context, msg message.Message) error {
	if s.fail.Load() {
		return markNonRetriableError(errors.New("injected persistence failure"))
	}
	return s.Service.Update(ctx, msg)
}

func TestSessionAgentPublishesCanonicalLiveEventOrder(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	agent := newLiveEventTestAgent(env, env.messages, hub, &scriptedLiveAgent{})
	sess, err := env.sessions.Create(t.Context(), "live events")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, TurnID: "turn-1", Prompt: "test", MaxOutputTokens: 100})
	require.NoError(t, err)
	events, err := hub.ReplayAfter(sess.ID, 0)
	require.NoError(t, err)
	require.Equal(t, []sessionevent.Kind{
		sessionevent.KindTurnStarted,
		sessionevent.KindMessageCreated,
		sessionevent.KindReasoningDelta,
		sessionevent.KindReasoningDelta,
		sessionevent.KindMessageDelta,
		sessionevent.KindToolProgress,
		sessionevent.KindToolProgress,
		sessionevent.KindToolCompleted,
		sessionevent.KindUsageUpdated,
		sessionevent.KindMessageCompleted,
		sessionevent.KindTurnCompleted,
	}, liveEventKinds(events))
	for index, event := range events {
		require.Equal(t, uint64(index+1), event.Sequence)
	}
	require.Equal(t, "hello", events[4].Payload.(sessionevent.TextDelta).Text)
	require.Equal(t, "file contents", events[7].Payload.(sessionevent.ToolEvent).Result)
	require.Equal(t, "turn-1", events[0].Payload.(sessionevent.TurnEvent).TurnID)
	require.Equal(t, "turn-1", events[len(events)-1].Payload.(sessionevent.TurnEvent).TurnID)
}

func TestSessionAgentPublishesBeforePersistenceFailure(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	messages := &failingUpdateMessageService{Service: env.messages}
	agent := newLiveEventTestAgent(env, messages, hub, &scriptedLiveAgent{failUpdates: &messages.fail})
	sess, err := env.sessions.Create(t.Context(), "persistence failure")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "test", MaxOutputTokens: 100})
	require.ErrorContains(t, err, "injected persistence failure")
	events, replayErr := hub.ReplayAfter(sess.ID, 0)
	require.NoError(t, replayErr)
	kinds := liveEventKinds(events)
	require.Contains(t, kinds, sessionevent.KindMessageDelta)
	require.Contains(t, kinds, sessionevent.KindMessageCompleted)
	require.Equal(t, sessionevent.KindTurnFailed, kinds[len(kinds)-1])
}

func TestSessionAgentTextDeltaPerformsNoPersistenceWrite(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	messages := &countingUpdateMessageService{Service: env.messages}
	deltaCounts := make(chan [2]int32, 1)
	agent := newLiveEventTestAgent(env, messages, hub, &scriptedLiveAgent{
		updateCount: &messages.updates,
		deltaCounts: deltaCounts,
	})
	sess, err := env.sessions.Create(t.Context(), "delta persistence")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "test", MaxOutputTokens: 100})
	require.NoError(t, err)
	counts := <-deltaCounts
	require.Equal(t, counts[0], counts[1], "text delta performed a synchronous persistence write")
}

func TestSessionAgentCancellationAcknowledgementPrecedesTerminalEvent(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	ready := make(chan struct{})
	agent := newLiveEventTestAgent(env, env.messages, hub, &scriptedLiveAgent{ready: ready, block: true})
	sess, err := env.sessions.Create(t.Context(), "cancel events")
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		_, runErr := agent.Run(context.Background(), SessionAgentCall{SessionID: sess.ID, Prompt: "test", MaxOutputTokens: 100})
		result <- runErr
	}()
	<-ready
	agent.Cancel(sess.ID)
	require.ErrorIs(t, <-result, context.Canceled)

	events, err := hub.ReplayAfter(sess.ID, 0)
	require.NoError(t, err)
	kinds := liveEventKinds(events)
	ackIndex := indexLiveEventKind(kinds, sessionevent.KindCancelAcknowledged)
	cancelIndex := indexLiveEventKind(kinds, sessionevent.KindTurnCancelled)
	require.NotEqual(t, -1, ackIndex)
	require.NotEqual(t, -1, cancelIndex)
	require.Less(t, ackIndex, cancelIndex)
}

func TestSessionAgentWithoutEventHubPreservesRunBehavior(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := newLiveEventTestAgent(env, env.messages, nil, &scriptedLiveAgent{})
	sess, err := env.sessions.Create(t.Context(), "no live hub")
	require.NoError(t, err)

	result, err := agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "test", MaxOutputTokens: 100})
	require.NoError(t, err)
	require.NotNil(t, result)
}

func newLiveEventTestAgent(env fakeEnv, messages message.Service, events SessionEventPublisher, scripted fantasy.Agent) SessionAgent {
	model := stubLanguageModel{stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return nil, errors.New("scripted agent must bypass language model")
	}}
	configuredModel := Model{
		Model: model,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200_000,
			DefaultMaxTokens: 1_000,
		},
		ModelCfg: config.SelectedModel{Provider: model.Provider(), Model: model.Model()},
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           configuredModel,
		SmallModel:           configuredModel,
		WorkingDir:           env.workingDir,
		Sessions:             env.sessions,
		Messages:             messages,
		DisableAutoSummarize: true,
		IsYolo:               true,
		SessionEvents:        events,
		RetryWaitFunc: func(context.Context, time.Duration) error {
			return nil
		},
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return scripted
		},
	})
}

func liveEventKinds(events []sessionevent.Event) []sessionevent.Kind {
	kinds := make([]sessionevent.Kind, len(events))
	for index, event := range events {
		kinds[index] = event.Kind
	}
	return kinds
}

func indexLiveEventKind(kinds []sessionevent.Kind, target sessionevent.Kind) int {
	for index, kind := range kinds {
		if kind == target {
			return index
		}
	}
	return -1
}

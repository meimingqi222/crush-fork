package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/stretchr/testify/require"
)

type queueTestAgent struct{}

func (queueTestAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

func (queueTestAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

type queuePrepareTestAgent struct {
	t                 *testing.T
	afterFirstPrepare func()
}

func (queuePrepareTestAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

func (a *queuePrepareTestAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	preparedCtx, prepared, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
	require.NoError(a.t, err)

	if a.afterFirstPrepare != nil {
		a.afterFirstPrepare()
		a.afterFirstPrepare = nil
	}

	_, _, err = call.PrepareStep(preparedCtx, fantasy.PrepareStepFunctionOptions{Messages: prepared.Messages})
	require.NoError(a.t, err)

	if call.OnTextDelta != nil {
		require.NoError(a.t, call.OnTextDelta("reply", "ok"))
	}
	if call.OnStepFinish != nil {
		require.NoError(a.t, call.OnStepFinish(fantasy.StepResult{
			Response: fantasy.Response{
				FinishReason: fantasy.FinishReasonStop,
			},
		}))
	}
	return &fantasy.AgentResult{}, nil
}

func newQueueControlTestAgent(env fakeEnv) *sessionAgent {
	return &sessionAgent{
		largeModel:         csync.NewValue(Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{}}),
		smallModel:         csync.NewValue(Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{}}),
		systemPromptPrefix: csync.NewValue(""),
		systemPrompt:       csync.NewValue(""),
		tools:              csync.NewSlice[fantasy.AgentTool](),
		agentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return queueTestAgent{}
		},
		sessions:        env.sessions,
		messages:        env.messages,
		messageQueue:    csync.NewMap[string, []SessionAgentCall](),
		steeringQueue:   csync.NewMap[string, []SessionAgentCall](),
		activeRequests:  csync.NewMap[string, context.CancelFunc](),
		pausedQueues:    csync.NewMap[string, bool](),
		steeringSignals: csync.NewMap[string, context.CancelFunc](),
	}
}

func newQueuePrepareTestSessionAgent(env fakeEnv, fakeAgent fantasy.Agent) *sessionAgent {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    10000,
			DefaultMaxTokens: 1000,
		},
	}

	return NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return fakeAgent
		},
	}).(*sessionAgent)
}

func TestResumeQueueStartsNextPromptWhenIdle(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "queue resume")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{{
		SessionID: sess.ID,
		Prompt:    "queued prompt",
	}})
	a.pausedQueues.Set(sess.ID, true)

	a.ResumeQueue(sess.ID)

	require.Eventually(t, func() bool {
		if a.QueuedPrompts(sess.ID) != 0 || a.IsSessionBusy(sess.ID) {
			return false
		}
		msgs, listErr := env.messages.List(t.Context(), sess.ID)
		if listErr != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.Role == message.User && msg.Content().Text == "queued prompt" {
				return true
			}
		}
		return false
	}, time.Second, 20*time.Millisecond)
	require.False(t, a.IsQueuePaused(sess.ID))
}

func TestResumeQueueDoesNotStartWhenBusy(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "queue busy")
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{{
		SessionID: sess.ID,
		Prompt:    "queued",
	}})
	a.pausedQueues.Set(sess.ID, true)
	a.activeRequests.Set(sess.ID, func() {})

	a.ResumeQueue(sess.ID)

	require.Equal(t, 1, a.QueuedPrompts(sess.ID))
	require.False(t, a.IsQueuePaused(sess.ID))
}

func TestCancelClearsQueuePauseState(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "queue cancel")
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{{
		SessionID: sess.ID,
		Prompt:    "queued",
	}})
	a.pausedQueues.Set(sess.ID, true)

	a.Cancel(sess.ID)

	require.Equal(t, 0, a.QueuedPrompts(sess.ID))
	require.False(t, a.IsQueuePaused(sess.ID))
}

func TestRemoveQueuedPromptClearsPauseWhenQueueEmpties(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "queue remove")
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{{
		SessionID: sess.ID,
		Prompt:    "queued",
	}})
	a.pausedQueues.Set(sess.ID, true)

	removed := a.RemoveQueuedPrompt(sess.ID, 0)
	require.True(t, removed)
	require.Equal(t, 0, a.QueuedPrompts(sess.ID))
	require.False(t, a.IsQueuePaused(sess.ID))
}

func TestQueuedPromptWaitsForCurrentRunByDefault(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	var sessionAgent *sessionAgent
	testAgent := &queuePrepareTestAgent{t: t}
	sessionAgent = newQueuePrepareTestSessionAgent(env, testAgent)

	sess, err := env.sessions.Create(t.Context(), "queue waits")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	sessionAgent.PauseQueue(sess.ID)
	testAgent.afterFirstPrepare = func() {
		_, runErr := sessionAgent.Run(context.Background(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "queued later",
			MaxOutputTokens: 1000,
		})
		require.NoError(t, runErr)
	}

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "run now",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, 1, sessionAgent.QueuedPrompts(sess.ID))

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, msg := range msgs {
		if msg.Role == message.User && msg.Content().Text == "queued later" {
			t.Fatalf("queued prompt was merged into the active run")
		}
	}
}

func TestJoinActiveRunQueuedPromptMergesIntoCurrentRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	var sessionAgent *sessionAgent
	testAgent := &queuePrepareTestAgent{t: t}
	sessionAgent = newQueuePrepareTestSessionAgent(env, testAgent)

	sess, err := env.sessions.Create(t.Context(), "queue joins run")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	sessionAgent.PauseQueue(sess.ID)
	testAgent.afterFirstPrepare = func() {
		_, runErr := sessionAgent.Run(context.Background(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "join now",
			JoinActiveRun:   true,
			MaxOutputTokens: 1000,
		})
		require.NoError(t, runErr)
	}

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "run now",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, 0, sessionAgent.QueuedPrompts(sess.ID))

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	foundJoinedPrompt := false
	for _, msg := range msgs {
		if msg.Role == message.User && strings.HasPrefix(msg.Content().Text, "join now") {
			foundJoinedPrompt = true
			break
		}
	}
	require.True(t, foundJoinedPrompt)
}

func TestJoinActiveRunQueuedPromptRespectsInjectionBudgets(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sessionAgent := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "join active budgets")
	require.NoError(t, err)

	sessionAgent.messageQueue.Set(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, Prompt: strings.Repeat("A", joinActiveRunPromptCharsBudget+200), JoinActiveRun: true},
		{SessionID: sess.ID, Prompt: "second", JoinActiveRun: true},
		{SessionID: sess.ID, Prompt: "third", JoinActiveRun: true},
	})

	calls := sessionAgent.takeJoinActiveRunCalls(sess.ID)
	require.Len(t, calls, 3)

	remaining := joinActiveRunPromptCharsBudget
	injected := 0
	for i := len(calls) - 1; i >= 0; i-- {
		call := calls[i]
		if injected >= joinActiveRunMaxInjectedCalls || remaining <= 0 {
			sessionAgent.enqueueQueuedCall(sess.ID, call)
			continue
		}
		prompt := strings.TrimSpace(call.Prompt)
		if prompt == "" {
			sessionAgent.enqueueQueuedCall(sess.ID, call)
			continue
		}
		runes := []rune(prompt)
		if len(runes) > remaining {
			if remaining <= 1 {
				sessionAgent.enqueueQueuedCall(sess.ID, call)
				continue
			}
			prompt = string(runes[:remaining-1]) + "…"
		}
		remaining -= len([]rune(prompt))
		injected++
	}

	require.Equal(t, joinActiveRunMaxInjectedCalls, injected)
	queue := sessionAgent.queuedCallsSnapshot(sess.ID)
	require.Len(t, queue, 1)
	require.Equal(t, strings.Repeat("A", joinActiveRunPromptCharsBudget+200), queue[0].Prompt)
}

func TestPrioritizeQueuedPromptMovesToJoinActiveRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "prioritize test")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, Prompt: "first"},
		{SessionID: sess.ID, Prompt: "second"},
		{SessionID: sess.ID, Prompt: "third"},
	})

	require.Equal(t, 3, a.QueuedPrompts(sess.ID))

	result := a.PrioritizeQueuedPrompt(sess.ID, 1)
	require.True(t, result)
	require.Equal(t, 3, a.QueuedPrompts(sess.ID))

	list := a.QueuedPromptsList(sess.ID)
	require.Equal(t, []string{"second", "first", "third"}, list)

	queueSnapshot, _ := a.messageQueue.Get(sess.ID)
	require.True(t, queueSnapshot[0].JoinActiveRun)
	require.False(t, queueSnapshot[1].JoinActiveRun)
	require.False(t, queueSnapshot[2].JoinActiveRun)
}

func TestPrioritizeQueuedPromptInvalidIndexReturnsFalse(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "prioritize invalid")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	a.messageQueue.Set(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, Prompt: "first"},
	})

	require.False(t, a.PrioritizeQueuedPrompt(sess.ID, -1))
	require.False(t, a.PrioritizeQueuedPrompt(sess.ID, 5))
}

func TestRemoveQueuedTurnAndEnqueueSteerUseStableTurnIdentity(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)
	sess, err := env.sessions.Create(t.Context(), "turn queue identity")
	require.NoError(t, err)
	a.activeRequests.Set(sess.ID, func() {})
	t.Cleanup(func() { a.activeRequests.Del(sess.ID) })

	require.True(t, a.EnqueueSteer(sess.ID, SessionAgentCall{TurnID: "steer-1", Prompt: "direction"}))
	// Steering messages ride the dedicated steering queue, never the regular
	// messageQueue, and are not JoinActiveRun prompts.
	require.Equal(t, 0, a.QueuedPrompts(sess.ID))
	steering := a.queuedSteeringSnapshot(sess.ID)
	require.Len(t, steering, 1)
	require.Equal(t, "steer-1", steering[0].TurnID)
	require.True(t, steering[0].Steering)
	require.False(t, steering[0].JoinActiveRun)
	require.True(t, a.RemoveQueuedTurn(sess.ID, "steer-1"))
	require.False(t, a.RemoveQueuedTurn(sess.ID, "steer-1"))
}

// TestSummarizeRejectsBusySessionWithoutInternalCompactionContext is a
// regression test for the external Summarize entry point's busy guard: an
// externally-triggered compaction request must still be rejected while the
// session has an active request, so it doesn't race with in-flight work.
func TestSummarizeRejectsBusySessionWithoutInternalCompactionContext(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	testSession, err := env.sessions.Create(t.Context(), "summarize busy without internal flag")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)
	concrete := agentUnderTest.(*sessionAgent)

	// Simulate a session that already has an active request in flight, as
	// would be the case right before the end-of-turn compaction path in Run
	// invokes Summarize.
	concrete.activeRequests.Set(testSession.ID, func() {})

	err = agentUnderTest.Summarize(t.Context(), testSession.ID, nil)
	require.ErrorIs(t, err, ErrSessionBusy)
	require.Equal(t, 0, fakeAgent.summaryCalls)
}

// TestSummarizeAllowsBusySessionWithInternalCompactionContext is a
// regression test for the end-of-turn compaction TOCTOU race fixed in Run:
// previously Run deleted the activeRequests entry before calling Summarize
// so Summarize's own busy check would pass, leaving a window where a queued
// prompt or a fresh Run for the same session could start concurrently. Now
// Run instead tags the context with internalCompactionKey (like the
// preflight-compaction path already did) and keeps the activeRequests entry
// held throughout. Summarize must honor that flag: skip the busy check, and
// leave the pre-existing activeRequests entry alone (Run is responsible for
// releasing it exactly once, after Summarize returns).
func TestSummarizeAllowsBusySessionWithInternalCompactionContext(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	testSession, err := env.sessions.Create(t.Context(), "summarize busy with internal flag")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)
	concrete := agentUnderTest.(*sessionAgent)

	concrete.activeRequests.Set(testSession.ID, func() {})

	ctx := context.WithValue(t.Context(), internalCompactionKey{}, true)
	err = agentUnderTest.Summarize(ctx, testSession.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, fakeAgent.summaryCalls)

	// Summarize must not have deleted the caller's activeRequests entry: it
	// skips its own Set/Del bookkeeping entirely when isInternalCompaction is
	// true, leaving release of the entry to the caller (Run).
	_, stillHeld := concrete.activeRequests.Get(testSession.ID)
	require.True(t, stillHeld, "activeRequests entry set before an internal-compaction Summarize call must still be held afterward")
}

func TestBusyRunRemovesPrecreatedUserMessageBeforeQueueing(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "queue precreated user message")
	require.NoError(t, err)

	queuedUser, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "queued but should not render yet"},
		},
	})
	require.NoError(t, err)

	a.activeRequests.Set(sess.ID, func() {})

	result, err := a.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued but should not render yet",
		MaxOutputTokens: 1000,
		UserMessage:     &queuedUser,
	})
	require.NoError(t, err)
	require.Nil(t, result)

	queue := a.queuedCallsSnapshot(sess.ID)
	require.Len(t, queue, 1)
	require.Nil(t, queue[0].UserMessage)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, msg := range msgs {
		if msg.ID == queuedUser.ID {
			t.Fatalf("queued user message %q should have been deleted while waiting", queuedUser.ID)
		}
	}
}

// steerCaptureTestAgent behaves like queuePrepareTestAgent but captures the
// prepared messages of the second PrepareStep so tests can assert what was
// injected mid-run.
type steerCaptureTestAgent struct {
	t                 *testing.T
	afterFirstPrepare func()
	secondPrepareMsgs []fantasy.Message
}

func (steerCaptureTestAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

func (a *steerCaptureTestAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	preparedCtx, prepared, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
	require.NoError(a.t, err)

	if a.afterFirstPrepare != nil {
		a.afterFirstPrepare()
		a.afterFirstPrepare = nil
	}

	_, prepared2, err := call.PrepareStep(preparedCtx, fantasy.PrepareStepFunctionOptions{Messages: prepared.Messages})
	require.NoError(a.t, err)
	a.secondPrepareMsgs = prepared2.Messages

	if call.OnTextDelta != nil {
		require.NoError(a.t, call.OnTextDelta("reply", "ok"))
	}
	if call.OnStepFinish != nil {
		require.NoError(a.t, call.OnStepFinish(fantasy.StepResult{
			Response: fantasy.Response{
				FinishReason: fantasy.FinishReasonStop,
			},
		}))
	}
	return &fantasy.AgentResult{}, nil
}

func TestSteerMessageIsInjectedIntoActiveRun(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testAgent := &steerCaptureTestAgent{t: t}
	sessionAgent := newQueuePrepareTestSessionAgent(env, testAgent)

	sess, err := env.sessions.Create(t.Context(), "steer inject")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	testAgent.afterFirstPrepare = func() {
		// The steer arrives while the run is busy, mid-tool-loop.
		require.True(t, sessionAgent.EnqueueSteer(sess.ID, SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "stop and fix the test first",
		}))
	}

	_, err = sessionAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "run now",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)

	require.NotEmpty(t, testAgent.secondPrepareMsgs)
	var userText strings.Builder
	for _, m := range testAgent.secondPrepareMsgs {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := part.(fantasy.TextPart); ok {
				userText.WriteString(tp.Text)
			}
		}
	}
	// The steer was injected into the next provider step, wrapped with the
	// priority notice so the model treats it as the active instruction.
	require.Contains(t, userText.String(), "stop and fix the test first")
	require.Contains(t, userText.String(), "<user_query>")
	require.Contains(t, userText.String(), "supersedes earlier directions")

	// The steering queue was drained: nothing is stranded for the next run.
	require.Empty(t, sessionAgent.queuedSteeringSnapshot(sess.ID))
	require.Equal(t, 0, sessionAgent.QueuedPrompts(sess.ID))
}

// TestEnqueueIRC_DoesNotSignalSteering covers docs/refactor-irc.md §2.2(b):
// unlike EnqueueSteer, a peer message must not cancel the session's
// cooperative steering signal -- a running tool (e.g. foreground bash) must
// not be told to yield just because a peer sent something. Contrasted
// directly against EnqueueSteer, which does cancel it, in the same test so a
// regression in either direction fails loudly.
func TestEnqueueIRC_DoesNotSignalSteering(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "irc no signal")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	a.steeringSignals.Set(sess.ID, cancel)
	a.activeRequests.Set(sess.ID, func() {}) // marks the session busy

	busy := a.EnqueueIRC(sess.ID, SessionAgentCall{Prompt: "peer message", PeerSteering: true, PeerFrom: "0-Main::peer-1"})
	require.True(t, busy)

	select {
	case <-ctx.Done():
		t.Fatal("EnqueueIRC must not cancel the session's steering signal")
	default:
	}
	require.Len(t, a.queuedSteeringSnapshot(sess.ID), 1, "the message must still land on the steering queue")

	// Contrast: a real user steer does cancel the signal.
	require.True(t, a.EnqueueSteer(sess.ID, SessionAgentCall{Prompt: "user steer"}))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("EnqueueSteer must still cancel the steering signal (regression guard for the contrast itself)")
	}
}

// TestEnqueueIRC_QueuesOnIdleSession covers the other half of EnqueueIRC's
// contract with EnqueueSteer: EnqueueSteer refuses to enqueue for an idle
// session (returns false, no-op), but EnqueueIRC must accept it -- this is
// what makes docs/refactor-irc.md §4.1(b)'s "idle primary agent: queue only"
// behavior fall out of the existing queue mechanics instead of needing a
// second, parallel pending-message store (§7).
func TestEnqueueIRC_QueuesOnIdleSession(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)

	sess, err := env.sessions.Create(t.Context(), "irc idle queue")
	require.NoError(t, err)

	require.False(t, a.EnqueueSteer(sess.ID, SessionAgentCall{Prompt: "should not enqueue"}),
		"EnqueueSteer must refuse an idle session")
	require.Empty(t, a.queuedSteeringSnapshot(sess.ID))

	busy := a.EnqueueIRC(sess.ID, SessionAgentCall{Prompt: "peer message to idle session", PeerSteering: true})
	require.False(t, busy, "the session was idle at enqueue time")
	require.Len(t, a.queuedSteeringSnapshot(sess.ID), 1, "EnqueueIRC must still enqueue for an idle session")
}

// TestPeerSteerMessageIsInjectedWithoutSupersedeWording is the peer-message
// counterpart of TestSteerMessageIsInjectedIntoActiveRun, guarding
// docs/refactor-irc.md §2.2(a): a peer message injected at the same drain
// point must not read as something that supersedes the model's current
// task, unlike a real user steer.
func TestPeerSteerMessageIsInjectedWithoutSupersedeWording(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testAgent := &steerCaptureTestAgent{t: t}
	sessionAgent := newQueuePrepareTestSessionAgent(env, testAgent)

	sess, err := env.sessions.Create(t.Context(), "peer steer inject")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "seed"},
		},
	})
	require.NoError(t, err)

	testAgent.afterFirstPrepare = func() {
		require.True(t, sessionAgent.EnqueueIRC(sess.ID, SessionAgentCall{
			SessionID:     sess.ID,
			Prompt:        "are you touching config.go?",
			PeerSteering:  true,
			PeerFrom:      "0-Main::peer-1",
			PeerMessageID: "msg-42",
		}))
	}

	_, err = sessionAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "run now",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)

	require.NotEmpty(t, testAgent.secondPrepareMsgs)
	var userText strings.Builder
	for _, m := range testAgent.secondPrepareMsgs {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := part.(fantasy.TextPart); ok {
				userText.WriteString(tp.Text)
			}
		}
	}
	text := userText.String()
	require.Contains(t, text, "are you touching config.go?")
	require.Contains(t, text, "<peer_message>")
	require.Contains(t, text, "0-Main::peer-1")
	require.Contains(t, text, "msg-42")
	require.NotContains(t, text, "supersedes",
		"a peer message must not read as overriding the model's current task priorities")
	require.NotContains(t, text, "<user_query>", "a peer message must not be framed as a user query")
}

func TestFlushStrandedSteeringMessagesPromotesToQueueFront(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := newQueueControlTestAgent(env)
	sess, err := env.sessions.Create(t.Context(), "flush steer")
	require.NoError(t, err)

	a.enqueueSteer(sess.ID, SessionAgentCall{SessionID: sess.ID, TurnID: "steer-1", Prompt: "stop"})
	a.messageQueue.Set(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, TurnID: "t-1", Prompt: "first queued"},
	})

	a.flushStrandedSteeringMessages(sess.ID)

	require.Empty(t, a.queuedSteeringSnapshot(sess.ID))
	queue := a.queuedCallsSnapshot(sess.ID)
	require.Len(t, queue, 2)
	require.Equal(t, "steer-1", queue[0].TurnID)
	require.Equal(t, "t-1", queue[1].TurnID)
}

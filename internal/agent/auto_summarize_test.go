package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/stretchr/testify/require"
)

type compactingPurposePlugin struct {
	purposes        []plugin.ChatTransformPurpose
	messagePurposes []plugin.ChatTransformPurpose
}

func (p *compactingPurposePlugin) Name() string { return "compacting-purpose-plugin" }

func (p *compactingPurposePlugin) Init(context.Context, plugin.PluginInput) (plugin.Hooks, error) {
	return plugin.Hooks{
		ChatMessagesTransform: func(_ context.Context, input plugin.ChatMessagesTransformInput, _ *plugin.ChatMessagesTransformOutput) error {
			p.messagePurposes = append(p.messagePurposes, input.Purpose)
			return nil
		},
		SessionCompacting: func(_ context.Context, input plugin.SessionCompactingInput, _ *plugin.SessionCompactingOutput) error {
			p.purposes = append(p.purposes, input.Purpose)
			return nil
		},
	}, nil
}

func (p *compactingPurposePlugin) Close(context.Context) error { return nil }

func initCompactingPurposePlugin(t *testing.T, env fakeEnv) *compactingPurposePlugin {
	tracker := &compactingPurposePlugin{}
	plugin.Register(tracker)
	err := plugin.Init(context.Background(), plugin.PluginInput{
		Sessions:   env.sessions,
		Messages:   env.messages,
		WorkingDir: env.workingDir,
	})
	require.NoError(t, err)
	return tracker
}

type requestPurposeCompactionPlugin struct{}

func (p *requestPurposeCompactionPlugin) Name() string { return "request-purpose-compaction-plugin" }

func (p *requestPurposeCompactionPlugin) Init(context.Context, plugin.PluginInput) (plugin.Hooks, error) {
	return plugin.Hooks{
		ChatMessagesTransform: func(_ context.Context, input plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			if input.RequestPurpose != plugin.ChatTransformPurposeRequest {
				return nil
			}
			output.Messages = []message.Message{{
				SessionID: input.SessionID,
				Role:      message.User,
				Parts:     []message.ContentPart{message.TextContent{Text: "tiny"}},
			}}
			return nil
		},
	}, nil
}

func (p *requestPurposeCompactionPlugin) Close(context.Context) error { return nil }

type requestPurposeSystemPrefixPlugin struct{}

func (p *requestPurposeSystemPrefixPlugin) Name() string {
	return "request-purpose-system-prefix-plugin"
}

func (p *requestPurposeSystemPrefixPlugin) Init(context.Context, plugin.PluginInput) (plugin.Hooks, error) {
	return plugin.Hooks{
		ChatSystemTransform: func(_ context.Context, input plugin.ChatSystemTransformInput, output *plugin.ChatSystemTransformOutput) error {
			if input.RequestPurpose != plugin.ChatTransformPurposeRequest {
				return nil
			}
			output.Prefix = output.Prefix + "\n# transformed"
			return nil
		},
	}, nil
}

func (p *requestPurposeSystemPrefixPlugin) Close(context.Context) error { return nil }

type autoSummarizeTestAgent struct {
	t                       *testing.T
	runCalls                int
	summaryCalls            int
	stepUsage               fantasy.Usage
	stepUsages              []fantasy.Usage
	afterPrepare            func()
	afterStep               func()
	runErr                  error
	runErrs                 []error
	summaryErrs             []error
	onSummary               func(fantasy.AgentStreamCall)
	errAfterStep            bool
	startToolBeforeRunError bool
	toolCallID              string
	toolName                string
}

func (a *autoSummarizeTestAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a *autoSummarizeTestAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		_, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
		require.NoError(a.t, err)
	}
	if a.afterPrepare != nil {
		a.afterPrepare()
	}

	isSummary := call.OnStepFinish == nil
	if isSummary {
		a.summaryCalls++
		if a.onSummary != nil {
			a.onSummary(call)
		}
		if len(a.summaryErrs) > 0 {
			summaryErr := a.summaryErrs[0]
			a.summaryErrs = a.summaryErrs[1:]
			if summaryErr != nil {
				return nil, summaryErr
			}
		}
		if call.OnTextDelta != nil {
			require.NoError(a.t, call.OnTextDelta("summary", "summary"))
		}
		return &fantasy.AgentResult{}, nil
	}

	a.runCalls++
	runErr := a.runErr
	if len(a.runErrs) > 0 {
		runErr = a.runErrs[0]
		a.runErrs = a.runErrs[1:]
	}
	if runErr != nil && !a.errAfterStep {
		if a.startToolBeforeRunError && call.OnToolInputStart != nil {
			toolCallID := a.toolCallID
			if toolCallID == "" {
				toolCallID = "tool-call-before-error"
			}
			toolName := a.toolName
			if toolName == "" {
				toolName = "view"
			}
			require.NoError(a.t, call.OnToolInputStart(toolCallID, toolName))
		}
		return nil, runErr
	}
	if call.OnTextDelta != nil {
		require.NoError(a.t, call.OnTextDelta("text", "ok"))
	}

	stepUsage := a.stepUsage
	if len(a.stepUsages) > 0 {
		stepUsage = a.stepUsages[0]
		a.stepUsages = a.stepUsages[1:]
	}

	stepResult := fantasy.StepResult{
		Response: fantasy.Response{
			FinishReason: fantasy.FinishReasonStop,
			Usage:        stepUsage,
		},
	}
	if call.OnStepFinish != nil {
		require.NoError(a.t, call.OnStepFinish(stepResult))
	}
	if a.afterStep != nil {
		a.afterStep()
	}
	if runErr != nil && a.errAfterStep {
		return nil, runErr
	}
	for _, cond := range call.StopWhen {
		if cond([]fantasy.StepResult{stepResult}) {
			break
		}
	}
	return &fantasy.AgentResult{}, nil
}

type failOnceMessageService struct {
	message.Service
	mu           sync.Mutex
	failNextList bool
}

func (s *failOnceMessageService) FailNextList() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextList = true
}

func (s *failOnceMessageService) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	s.mu.Lock()
	shouldFail := s.failNextList
	if shouldFail {
		s.failNextList = false
	}
	s.mu.Unlock()
	if shouldFail {
		return nil, errors.New("forced list failure")
	}
	return s.Service.List(ctx, sessionID)
}

func newAutoSummarizeTestSessionAgent(_ *testing.T, env fakeEnv, fakeAgent fantasy.Agent, messages message.Service, contextWindow int64) SessionAgent {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    contextWindow,
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
		Messages:     messages,
		AgentFactory: func(model fantasy.LanguageModel, opts ...fantasy.AgentOption) fantasy.Agent {
			return fakeAgent
		},
		RetryWaitFunc: func(context.Context, time.Duration) error {
			return nil
		},
	})
}

func TestRunPrepareStepPublishesProvisionalAssistantUsage(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "prepare-step provisional usage")
	require.NoError(t, err)

	var provisional message.Usage
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		stepUsage: fantasy.Usage{
			InputTokens:  120,
			OutputTokens: 25,
		},
		afterPrepare: func() {
			msgs, listErr := env.messages.List(t.Context(), testSession.ID)
			require.NoError(t, listErr)

			found := false
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role != message.Assistant {
					continue
				}
				provisional = msgs[i].Usage
				found = true
				break
			}
			require.True(t, found)
			require.Greater(t, provisional.InputTokens, int64(0))
			require.Zero(t, provisional.OutputTokens)
			require.Nil(t, msgs[len(msgs)-1].FinishPart())
		},
	}
	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 50_000)

	result, err := agentUnderTest.Run(t.Context(), SessionAgentCall{
		Prompt:          "Explain why the prompt was compacted.",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Greater(t, provisional.InputTokens, int64(0))
}

func TestRunFallbackEstimateIncludesSystemPromptAndUserPrompt(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "run estimate")
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 50_000)
	concrete := agentUnderTest.(*sessionAgent)

	const (
		systemPrompt = "You are a careful coding agent."
		promptPrefix = "Follow the workspace instructions exactly."
		userPrompt   = "Explain why this session hit the context limit."
	)
	concrete.SetSystemPrompt(systemPrompt)
	concrete.SetSystemPromptPrefix(promptPrefix)

	_, err = agentUnderTest.Run(t.Context(), SessionAgentCall{
		Prompt:          userPrompt,
		SessionID:       testSession.ID,
		MaxOutputTokens: 1_000,
	})
	require.NoError(t, err)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)

	historyWithPrefix := []fantasy.Message{fantasy.NewSystemMessage(promptPrefix)}
	expected := concrete.estimateSessionPromptTokens(
		historyWithPrefix,
		userPrompt,
		nil,
		nil,
		systemPrompt,
		"",
	)
	require.Equal(t, expected, savedSession.LastPromptTokens)
}

func TestSummarizeFallbackEstimateIncludesFullSummaryRequest(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summary estimate")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Summarize the long discussion about provider token accounting."}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 50_000)
	concrete := agentUnderTest.(*sessionAgent)

	const promptPrefix = "Compact aggressively but keep key decisions."
	concrete.SetSystemPromptPrefix(promptPrefix)

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)
	aiMsgs, _ := concrete.preparePrompt(msgs)

	expected := concrete.estimateSessionPromptTokens(
		aiMsgs,
		buildSessionCompactingPrompt(nil, nil, ""),
		nil,
		nil,
		string(summaryPrompt),
		promptPrefix,
	)

	err = agentUnderTest.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, expected, savedSession.LastPromptTokens)
}

func TestSummarizePublishesProvisionalSummaryUsage(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summary provisional usage")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("summarize this context ", 200)}},
	})
	require.NoError(t, err)

	var provisional message.Usage
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		afterPrepare: func() {
			msgs, listErr := env.messages.List(t.Context(), testSession.ID)
			require.NoError(t, listErr)

			found := false
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role != message.Assistant || !msgs[i].IsSummaryMessage {
					continue
				}
				provisional = msgs[i].Usage
				require.Greater(t, provisional.InputTokens, int64(0))
				require.Zero(t, provisional.OutputTokens)
				require.Nil(t, msgs[i].FinishPart())
				found = true
				break
			}
			require.True(t, found)
		},
	}

	agentUnderTest := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 50_000)
	err = agentUnderTest.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)
	require.Greater(t, provisional.InputTokens, int64(0))
}

func TestRunPreflightAutoSummarizesBeforeRequest(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	purposeTracker := initCompactingPurposePlugin(t, env)
	testSession, err := env.sessions.Create(t.Context(), "preflight summarize")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("x", 40000)}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.Equal(t, 1, fakeAgent.runCalls)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeMicroCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeCollapse)
	require.Contains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeReactiveCompact)
	require.Equal(t, []plugin.ChatTransformPurpose{plugin.ChatTransformPurposeProactiveCompact}, purposeTracker.purposes)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunPreflightAutoSummarizesWhenLastInputTokensAlreadyNearThreshold(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "preflight summarize by last input")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "small history"}},
	})
	require.NoError(t, err)

	testSession.LastPromptTokens = 168_000
	_, err = env.sessions.Save(t.Context(), testSession)
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.Equal(t, 1, fakeAgent.runCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunPreflightEstimateTrustsPluginCompactionForRequestPurpose(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	plugin.Register(&requestPurposeCompactionPlugin{})
	err := plugin.Init(context.Background(), plugin.PluginInput{
		Sessions:   env.sessions,
		Messages:   env.messages,
		WorkingDir: env.workingDir,
	})
	require.NoError(t, err)

	testSession, err := env.sessions.Create(t.Context(), "preflight plugin compaction")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("x", 30000)}},
	})
	require.NoError(t, err)

	testSession.LastPromptTokens = 9_500
	_, err = env.sessions.Save(t.Context(), testSession)
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10_000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	// LastInputTokens (9_500) is the observed API count and must always
	// serve as the floor for compaction decisions, regardless of whether
	// a plugin transform reduced the character-based estimate. With a
	// 10_000 window, usable budget is 5_000, so 9_500 triggers summarization.
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.Equal(t, 1, fakeAgent.runCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunPreflightEstimateKeepsLastInputFallbackWhenTransformDoesNotReduceEstimate(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	plugin.Register(&requestPurposeSystemPrefixPlugin{})
	err := plugin.Init(context.Background(), plugin.PluginInput{
		Sessions:   env.sessions,
		Messages:   env.messages,
		WorkingDir: env.workingDir,
	})
	require.NoError(t, err)

	testSession, err := env.sessions.Create(t.Context(), "preflight transform without reduction")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "small history"}},
	})
	require.NoError(t, err)

	testSession.LastPromptTokens = 168_000
	_, err = env.sessions.Save(t.Context(), testSession)
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.Equal(t, 1, fakeAgent.runCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunStepAutoSummarizesWhenEstimateFallbackExceedsThreshold(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "step summarize fallback")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	messages := &failOnceMessageService{Service: env.messages}
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		stepUsage: fantasy.Usage{
			InputTokens:  9500,
			OutputTokens: 10,
		},
		afterStep: messages.FailNextList,
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, messages, 10000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunStepAutoSummarizeFallbackIgnoresOutputTokens(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "step summarize ignores output")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	messages := &failOnceMessageService{Service: env.messages}
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		stepUsage: fantasy.Usage{
			InputTokens:  4000,
			OutputTokens: 9000,
		},
		afterStep: messages.FailNextList,
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, messages, 10000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.runCalls)
	require.Equal(t, 0, fakeAgent.summaryCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Empty(t, savedSession.SummaryMessageID)
	require.Equal(t, int64(4000), savedSession.LastInputTokens())
	require.Equal(t, int64(9000), savedSession.LastOutputTokens())
}

func TestRunStepAutoSummarizeFallbackKeepsLastInputWhenTransformDoesNotReduceEstimate(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	plugin.Register(&requestPurposeSystemPrefixPlugin{})
	err := plugin.Init(context.Background(), plugin.PluginInput{
		Sessions:   env.sessions,
		Messages:   env.messages,
		WorkingDir: env.workingDir,
	})
	require.NoError(t, err)

	testSession, err := env.sessions.Create(t.Context(), "step summarize transform without reduction")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	messages := &failOnceMessageService{Service: env.messages}
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		stepUsage: fantasy.Usage{
			InputTokens:  9500,
			OutputTokens: 10,
		},
		afterStep: messages.FailNextList,
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, messages, 10000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunTransientRetryNearContextLimitSummarizesInsteadOfRetrying(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	purposeTracker := initCompactingPurposePlugin(t, env)
	testSession, err := env.sessions.Create(t.Context(), "retry summarize")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		stepUsages: []fantasy.Usage{
			{
				InputTokens:  168_000,
				OutputTokens: 10,
			},
			{
				InputTokens:  100,
				OutputTokens: 10,
			},
		},
		runErrs: []error{
			&fantasy.ProviderError{
				StatusCode: 503,
				Message:    "service temporarily unavailable",
			},
		},
		errAfterStep: true,
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeMicroCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeCollapse)
	require.Contains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeReactiveCompact)
	require.Equal(t, []plugin.ChatTransformPurpose{plugin.ChatTransformPurposeRecover}, purposeTracker.purposes)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
	require.Equal(t, int64(100), savedSession.LastInputTokens())
}

func TestRunStreamingContextWindowErrorStringForcesSummarizeRecovery(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	purposeTracker := initCompactingPurposePlugin(t, env)
	testSession, err := env.sessions.Create(t.Context(), "streaming context-window recover")
	require.NoError(t, err)

	const toolCallID = "tool-call-before-overflow"
	fakeAgent := &autoSummarizeTestAgent{
		t:                       t,
		startToolBeforeRunError: true,
		toolCallID:              toolCallID,
		runErrs: []error{
			errors.New("received error while streaming: {\"message\":\"{\\\"error\\\":{\\\"message\\\":\\\"Your input exceeds the context window of this model. Please adjust your input and try again.\\\",\\\"code\\\":\\\"invalid_request_body\\\"}}\"}"),
			nil,
		},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeMicroCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeCollapse)
	require.Contains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeReactiveCompact)
	require.Equal(t, []plugin.ChatTransformPurpose{plugin.ChatTransformPurposeRecover}, purposeTracker.purposes)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)
	var foundToolCall bool
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls() {
			if tc.ID == toolCallID {
				foundToolCall = true
				require.True(t, tc.Finished)
			}
		}
	}
	require.True(t, foundToolCall)

	var foundSyntheticToolResult bool
	for _, msg := range msgs {
		for _, tr := range msg.ToolResults() {
			if tr.ToolCallID == toolCallID {
				foundSyntheticToolResult = true
				require.True(t, tr.IsError)
				require.Contains(t, tr.Content, "error while executing the tool")
			}
		}
	}
	require.True(t, foundSyntheticToolResult)

	// Verify the failed assistant message has usage set to the context
	// window so the UI shows a meaningful 100% instead of a stale
	// underestimated value.
	var failedAssistant *message.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant && msgs[i].FinishReason() == message.FinishReasonError {
			failedAssistant = &msgs[i]
			break
		}
	}
	require.NotNil(t, failedAssistant)
	require.Equal(t, int64(200_000), failedAssistant.Usage.InputTokens)
	require.Equal(t, int64(0), failedAssistant.Usage.OutputTokens)
}

func TestRunStreamingContextWindowRecoveryOnlyOnce(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "streaming context-window no loop")
	require.NoError(t, err)

	streamErr := errors.New("received error while streaming: {\"message\":\"{\\\"error\\\":{\\\"message\\\":\\\"Your input exceeds the context window of this model. Please adjust your input and try again.\\\",\\\"code\\\":\\\"invalid_request_body\\\"}}\"}")
	fakeAgent := &autoSummarizeTestAgent{
		t:       t,
		runErrs: []error{streamErr, streamErr},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	_, err = sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "context window")
	require.Equal(t, 2, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)
}

func TestRunNormalSummarizeUsesSummarizePurpose(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	purposeTracker := initCompactingPurposePlugin(t, env)
	testSession, err := env.sessions.Create(t.Context(), "normal summarize")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		afterStep: func() {
			_, createErr := env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{ID: "tc-1", Name: "view", Input: "{}"},
				},
			})
			require.NoError(t, createErr)
			_, createErr = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{ToolCallID: "tc-1", Name: "view", Content: strings.Repeat("x", 30000)},
				},
			})
			require.NoError(t, createErr)
		},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeMicroCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeCollapse)
	require.Contains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeAutoCompact)
	require.Contains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposePostCompact)
	require.Equal(t, []plugin.ChatTransformPurpose{plugin.ChatTransformPurposeSummarize}, purposeTracker.purposes)

	savedSession, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, savedSession.SummaryMessageID)
}

func TestRunContextWindowErrorAfterCompletedStepSummarizesAndAutoResumes(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "context-window after step")
	require.NoError(t, err)

	contextWindowErr := &fantasy.ProviderError{
		StatusCode: 400,
		Message:    "Your input exceeds the context window of this model.",
	}
	fakeAgent := &autoSummarizeTestAgent{
		t:            t,
		runErrs:      []error{contextWindowErr, nil},
		errAfterStep: true,
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 200_000)

	_, err = sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.Equal(t, 2, fakeAgent.runCalls)
	require.Equal(t, 1, fakeAgent.summaryCalls)
}

func TestRunAfterPriorSummaryKeepsOriginalPromptAcrossRecoverResume(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "switch then recover")
	require.NoError(t, err)
	createSeedHistoryMessage(t, env, testSession.ID)

	contextWindowErr := &fantasy.ProviderError{
		StatusCode: 400,
		Message:    "Your input exceeds the context window of this model.",
	}
	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 1_000_000)

	err = sessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, fakeAgent.summaryCalls)

	smallModel := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200_000,
			DefaultMaxTokens: 50_000,
		},
	}
	sessionAgent.SetModels(smallModel, smallModel)
	fakeAgent.runErrs = []error{contextWindowErr, nil}
	fakeAgent.errAfterStep = true

	const prompt = "continue after model switch"
	result, err := sessionAgent.Run(t.Context(), SessionAgentCall{
		Prompt:          prompt,
		SessionID:       testSession.ID,
		MaxOutputTokens: 50_000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, fakeAgent.runCalls)
	require.Equal(t, 2, fakeAgent.summaryCalls)

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)

	var promptCount int
	for _, msg := range msgs {
		if msg.Role != message.User {
			continue
		}
		if msg.Content().Text == prompt {
			promptCount++
		}
	}
	require.Equal(t, 1, promptCount)
}

func TestSummarizeSkipsAutoCompactForTinyHistory(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	purposeTracker := initCompactingPurposePlugin(t, env)
	testSession, err := env.sessions.Create(t.Context(), "tiny summarize")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "only one"}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{t: t}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	err = sessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, fakeAgent.summaryCalls)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeMicroCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeCollapse)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposeAutoCompact)
	require.NotContains(t, purposeTracker.messagePurposes, plugin.ChatTransformPurposePostCompact)
}

func TestSummarizeRetryableErrorRetriesAndSucceeds(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summarize retry success")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("x", 4000)}},
	})
	require.NoError(t, err)

	fakeAgent := &autoSummarizeTestAgent{
		t:           t,
		summaryErrs: []error{&fantasy.ProviderError{StatusCode: 503, Message: "service unavailable"}, nil},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	err = sessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 2, fakeAgent.summaryCalls)

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)
	var summaryMsg *message.Message
	for i := range msgs {
		if msgs[i].IsSummaryMessage {
			summaryMsg = &msgs[i]
		}
	}
	require.NotNil(t, summaryMsg)
	require.Equal(t, message.FinishReasonEndTurn, summaryMsg.FinishReason())
}

func TestSummarizeRetryableErrorExhaustedMarksFailure(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summarize retry failed")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("x", 4000)}},
	})
	require.NoError(t, err)

	summaryErrs := make([]error, maxRetriableAttempts+1)
	for i := range summaryErrs {
		summaryErrs[i] = &fantasy.ProviderError{StatusCode: 503, Message: "service unavailable"}
	}
	fakeAgent := &autoSummarizeTestAgent{t: t, summaryErrs: summaryErrs}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	start := time.Now()
	err = sessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.Error(t, err)
	require.Equal(t, maxRetriableAttempts+1, fakeAgent.summaryCalls)
	require.Less(t, time.Since(start), 2*time.Second)

	msgs, listErr := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, listErr)
	var summaryMsg *message.Message
	for i := range msgs {
		if msgs[i].IsSummaryMessage {
			summaryMsg = &msgs[i]
		}
	}
	require.NotNil(t, summaryMsg)
	require.Equal(t, message.FinishReasonError, summaryMsg.FinishReason())
	require.Equal(t, "Summarization failed", summaryMsg.FinishPart().Message)
	require.Contains(t, summaryMsg.FinishPart().Details, "Retried")
}

func TestSummarizeRetriesWithoutRedactedThinkingOnAnthropicProxyError(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summarize redacted thinking retry")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "use the view tool"}},
	})
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "tool reasoning"},
			message.ToolCall{ID: "call-1", Name: "view", Input: `{"file_path":"README.md","offset":0,"limit":120}`},
		},
	})
	require.NoError(t, err)

	sawRedacted := make([]bool, 0, 2)
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		summaryErrs: []error{
			&fantasy.ProviderError{
				StatusCode: 422,
				Message:    `'redacted_thinking' is not a valid content block type`,
			},
			nil,
		},
		onSummary: func(call fantasy.AgentStreamCall) {
			redacted := false
			for _, msg := range call.Messages {
				for _, part := range msg.Content {
					rp, ok := part.(fantasy.ReasoningPart)
					if !ok {
						continue
					}
					if isAnthropicRedactedReasoning(rp) {
						redacted = true
					}
				}
			}
			sawRedacted = append(sawRedacted, redacted)
		},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	err = sessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 2, fakeAgent.summaryCalls)
	require.Equal(t, []bool{true, false}, sawRedacted)
}

func TestSummarizeRetriesWithoutAnthropicThinkingOnUnsignedReasoningError(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summarize anthropic thinking retry")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "open the file"}},
	})
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{
				Thinking:  "signed reasoning",
				Signature: base64.StdEncoding.EncodeToString([]byte("sig")),
			},
			message.ToolCall{ID: "call-2", Name: "view", Input: `{"file_path":"README.md","offset":0,"limit":120}`},
		},
	})
	require.NoError(t, err)

	thinkingStates := make([]bool, 0, 2)
	fakeAgent := &autoSummarizeTestAgent{
		t: t,
		summaryErrs: []error{
			&fantasy.ProviderError{
				StatusCode: 400,
				Message:    "thinking is enabled but reasoning_content is missing in assistant tool call message at index 1",
			},
			nil,
		},
		onSummary: func(call fantasy.AgentStreamCall) {
			anthropicOpts, ok := call.ProviderOptions[anthropic.Name].(*anthropic.ProviderOptions)
			hasThinking := ok && anthropicOpts != nil && anthropicOpts.Thinking != nil
			thinkingStates = append(thinkingStates, hasThinking)
		},
	}
	sessionAgent := newAutoSummarizeTestSessionAgent(t, env, fakeAgent, env.messages, 10000)

	err = sessionAgent.Summarize(t.Context(), testSession.ID, fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderOptions{
			Thinking: &anthropic.ThinkingProviderOption{BudgetTokens: 2000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 2, fakeAgent.summaryCalls)
	require.Equal(t, []bool{true, false}, thinkingStates)
}

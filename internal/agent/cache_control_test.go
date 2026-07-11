package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// cacheControlCapturingAgent captures the messages produced by PrepareStep so
// tests can inspect cache control placement.
type cacheControlCapturingAgent struct {
	t          *testing.T
	prepared   [][]fantasy.Message
	finishStop bool
}

// cacheControlLanguageModel is a minimal fantasy.LanguageModel stub used as the
// model field while the capturing agent intercepts the agent stream.
type cacheControlLanguageModel struct{}

func (cacheControlLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{}, nil
}

func (cacheControlLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {}, nil
}

func (cacheControlLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return &fantasy.ObjectResponse{}, nil
}

func (cacheControlLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return func(yield func(fantasy.ObjectStreamPart) bool) {}, nil
}

func (cacheControlLanguageModel) Provider() string { return "test" }
func (cacheControlLanguageModel) Model() string    { return "test" }

func (cacheControlCapturingAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

func (a *cacheControlCapturingAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	_, prepared, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
	require.NoError(a.t, err)
	a.prepared = append(a.prepared, append([]fantasy.Message(nil), prepared.Messages...))

	if call.OnTextDelta != nil {
		require.NoError(a.t, call.OnTextDelta("assistant", "ok"))
	}
	if call.OnStepFinish != nil {
		reason := fantasy.FinishReasonToolCalls
		if a.finishStop {
			reason = fantasy.FinishReasonStop
		}
		require.NoError(a.t, call.OnStepFinish(fantasy.StepResult{Response: fantasy.Response{FinishReason: reason}}))
	}

	return &fantasy.AgentResult{}, nil
}

func hasCacheControl(msg fantasy.Message) bool {
	opts, ok := msg.ProviderOptions[anthropic.Name]
	if !ok {
		return false
	}
	cc, ok := opts.(*anthropic.ProviderCacheControlOptions)
	if !ok {
		return false
	}
	return cc.CacheControl.Type == "ephemeral"
}

func TestCacheControlPeriodicBreakpoints(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "Cache Breakpoints")
	require.NoError(t, err)

	// Build a conversation with a system prompt and 10 user/assistant turns.
	msgs := []message.Message{
		{Role: message.System, Parts: []message.ContentPart{message.TextContent{Text: "You are a helpful assistant."}}},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "question"}}},
			message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "answer"}}},
		)
	}
	for _, m := range msgs {
		_, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  m.Role,
			Parts: m.Parts,
		})
		require.NoError(t, err)
	}

	capturingAgent := &cacheControlCapturingAgent{t: t, finishStop: true}
	model := Model{
		Model: cacheControlLanguageModel{},
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
		ModelCfg: config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4"},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return capturingAgent
		},
	}).(*sessionAgent)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "next question",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.Len(t, capturingAgent.prepared, 1)

	prepared := capturingAgent.prepared[0]
	var cacheIndices []int
	for i, msg := range prepared {
		if hasCacheControl(msg) {
			cacheIndices = append(cacheIndices, i)
		}
	}

	// Main system prompt + last 2 messages + at most 1 periodic boundary.
	// Total must not exceed maxCacheBreakpoints (4).
	require.NotEmpty(t, cacheIndices)
	require.Equal(t, 0, cacheIndices[0], "main system prompt should be cached")
	require.Contains(t, cacheIndices, len(prepared)-1, "last message should be cached")
	require.Contains(t, cacheIndices, len(prepared)-2, "second-to-last message should be cached")
	require.LessOrEqual(t, len(cacheIndices), maxCacheBreakpoints, "breakpoint count must not exceed Anthropic's limit")
}

func TestAutoModeReminderAsPromptSuffix(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "Auto Mode Reminder Suffix")
	require.NoError(t, err)
	_, err = env.sessions.UpdatePermissionMode(t.Context(), sess.ID, session.PermissionModeAuto)
	require.NoError(t, err)

	// Seed a prior full reminder plus enough assistant turns so the next
	// reminder would be sparse, then add a pending sparse auto-mode prompt
	// message. buildChatRequestState should extract it into PromptSuffix.
	basePrompt := message.AutoModePromptContent(message.AutoModePromptTypeFull)
	msgs := []message.Message{
		{Role: message.System, Parts: []message.ContentPart{message.TextContent{Text: basePrompt}}},
	}
	for range autoModeReminderTurnsBetween {
		msgs = append(msgs, message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "progress"}}})
	}
	msgs = append(msgs, message.Message{
		Role:  message.System,
		Parts: []message.ContentPart{message.TextContent{Text: message.AutoModePromptContent(message.AutoModePromptTypeSparse)}},
	})
	for _, m := range msgs {
		_, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  m.Role,
			Parts: m.Parts,
		})
		require.NoError(t, err)
	}

	agent := testSessionAgent(env, nil, nil, "You are a helpful assistant.").(*sessionAgent)
	state, err := agent.buildChatRequestState(t.Context(), chatRequestStateInput{
		SessionID:      sess.ID,
		Agent:          "session",
		Model:          agent.largeModel.Get(),
		Provider:       defaultProviderContext(),
		Purpose:        plugin.ChatTransformPurposeRequest,
		RequestPurpose: plugin.ChatTransformPurposeRequest,
		Messages:       msgs,
		Message:        message.Message{SessionID: sess.ID, Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}},
		SystemPrompt:   "You are a helpful assistant.",
		PermissionMode: sess.PermissionMode,
	})
	require.NoError(t, err)

	require.NotEmpty(t, state.PromptSuffix, "auto-mode reminder should be placed in PromptSuffix")
	require.Contains(t, state.PromptSuffix, "Auto mode", "reminder should mention auto mode")
	require.False(t, strings.Contains(state.SystemPrompt, "Auto mode"), "main system prompt should remain stable and not contain the reminder")
}

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type promptEnhanceLanguageModel struct {
	provider string
	model    string
	stream   func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
}

func (m promptEnhanceLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	panic("unexpected Generate call")
}

func (m promptEnhanceLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.stream != nil {
		return m.stream(ctx, call)
	}
	panic("unexpected Stream call")
}

func (m promptEnhanceLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("unexpected GenerateObject call")
}

func (m promptEnhanceLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("unexpected StreamObject call")
}

func (m promptEnhanceLanguageModel) Provider() string { return m.provider }
func (m promptEnhanceLanguageModel) Model() string    { return m.model }

func TestEnhancePromptModelPrefersSmallConfiguredModel(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	want := cfg.Config().Models[config.SelectedModelTypeSmall]
	providerCfg, ok := cfg.Config().Providers.Get(want.Provider)
	require.True(t, ok)

	coord := &coordinator{cfg: cfg}
	got, gotProvider, err := coord.enhancePromptModel(t.Context())
	require.NoError(t, err)
	require.Equal(t, want.Model, got.ModelCfg.Model)
	require.Equal(t, want.Provider, got.ModelCfg.Provider)
	require.Equal(t, providerCfg.ID, gotProvider.ID)
}

func TestLoadEnhancePromptHistorySkipsToolsAndHonorsSummaryBoundary(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "enhance")
	require.NoError(t, err)

	oldUser, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "old user"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old assistant"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: "tool result"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "new user"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "new assistant"}},
	})
	require.NoError(t, err)

	sess.SummaryMessageID = oldUser.ID
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	coord := &coordinator{messages: env.messages, sessions: env.sessions}
	history := coord.loadEnhancePromptHistory(t.Context(), sess.ID, Model{CatwalkCfg: catwalk.Model{ContextWindow: 64_000}})

	require.Len(t, history, 4)
	require.Equal(t, fantasy.MessageRoleUser, history[0].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, history[1].Role)
	require.Equal(t, fantasy.MessageRoleUser, history[2].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, history[3].Role)

	part0, ok := fantasy.AsContentType[fantasy.TextPart](history[0].Content[0])
	require.True(t, ok)
	require.Equal(t, "old user", part0.Text)
	part1, ok := fantasy.AsContentType[fantasy.TextPart](history[1].Content[0])
	require.True(t, ok)
	require.Equal(t, "old assistant", part1.Text)
	part2, ok := fantasy.AsContentType[fantasy.TextPart](history[2].Content[0])
	require.True(t, ok)
	require.Equal(t, "new user", part2.Text)
	part3, ok := fantasy.AsContentType[fantasy.TextPart](history[3].Content[0])
	require.True(t, ok)
	require.Equal(t, "new assistant", part3.Text)
}

func TestLoadEnhancePromptHistoryStripsToolCallsToAvoidOrphans(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "enhance toolcalls")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{ID: "call_1", Name: "view", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_1", Name: "view", Content: "ok"},
		},
	})
	require.NoError(t, err)
	// Assistant turn with ONLY a tool call (no text). This must be dropped
	// to avoid sending an empty assistant message to providers.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call_2", Name: "grep", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_2", Name: "grep", Content: "ok"},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)

	coord := &coordinator{messages: env.messages, sessions: env.sessions}
	history := coord.loadEnhancePromptHistory(t.Context(), sess.ID, Model{CatwalkCfg: catwalk.Model{ContextWindow: 64_000}})

	// Expect: user "hi", assistant "let me check" (tool call stripped),
	// assistant "done". The assistant-only-tool-call turn and both tool
	// messages are dropped.
	require.Len(t, history, 3)
	require.Equal(t, fantasy.MessageRoleUser, history[0].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, history[1].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, history[2].Role)

	for _, msg := range history {
		require.NotEqual(t, fantasy.MessageRoleTool, msg.Role)
		for _, part := range msg.Content {
			_, isCall := part.(fantasy.ToolCallPart)
			require.False(t, isCall, "history must not contain ToolCallPart")
			_, isCallPtr := part.(*fantasy.ToolCallPart)
			require.False(t, isCallPtr, "history must not contain *ToolCallPart")
		}
	}

	textPart, ok := fantasy.AsContentType[fantasy.TextPart](history[1].Content[0])
	require.True(t, ok)
	require.Equal(t, "let me check", textPart.Text)
}

func TestLoadEnhancePromptHistoryTruncatesToAugmentBudget(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "enhance budget")
	require.NoError(t, err)

	for range 12 {
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "1234567890abcdefghij1234567890abcdefghij1234567890abcdefghij1234567890abcdefghij"}},
		})
		require.NoError(t, err)
	}

	coord := &coordinator{messages: env.messages, sessions: env.sessions}
	history := coord.loadEnhancePromptHistory(t.Context(), sess.ID, Model{CatwalkCfg: catwalk.Model{ContextWindow: 80}})

	require.NotEmpty(t, history)
	require.Less(t, len(history), 12)
	require.LessOrEqual(t, estimatePromptTokens(history, nil), augmentEnhancePromptBudget(Model{CatwalkCfg: catwalk.Model{ContextWindow: 80}}, 0))
}

func TestAugmentEnhancePromptBudgetUsesEffectiveContextWindow(t *testing.T) {
	t.Parallel()

	model := Model{CatwalkCfg: catwalk.Model{ContextWindow: 10_000}}
	require.Equal(t, int64(8_976), augmentEnhancePromptBudget(model, 1_024))

	fallback := augmentEnhancePromptBudget(Model{}, 1_024)
	require.Equal(t, int64(23_976), fallback)
}

func TestEnhancePromptTemplatePreservesLanguageInstruction(t *testing.T) {
	t.Parallel()

	require.Contains(t, enhancePromptTemplate, "SAME natural language")
	require.Contains(t, enhancePromptTemplate, "Do not translate")
}

func TestEnhancePromptSendsInitialMessagesWithoutPrompt(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		require.NoError(t, err)

		messages, ok := body["messages"].([]any)
		require.True(t, ok)
		require.Len(t, messages, 1)
		messageObj, ok := messages[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "user", messageObj["role"])
		require.Contains(t, messageObj["content"], "你是谁")
		require.Equal(t, cfg.Config().Models[config.SelectedModelTypeSmall].Model, body["model"])

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1702657020,\"model\":\"" + cfg.Config().Models[config.SelectedModelTypeSmall].Model + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1702657020,\"model\":\"" + cfg.Config().Models[config.SelectedModelTypeSmall].Model + "\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<augment-enhanced-prompt>增强结果</augment-enhanced-prompt>\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1702657020,\"model\":\"" + cfg.Config().Models[config.SelectedModelTypeSmall].Model + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1702657020,\"model\":\"" + cfg.Config().Models[config.SelectedModelTypeSmall].Model + "\",\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":5,\"total_tokens\":22}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	smallCfg := cfg.Config().Models[config.SelectedModelTypeSmall]
	cfg.Config().Providers.Set(smallCfg.Provider, config.ProviderConfig{
		ID:      smallCfg.Provider,
		Type:    openaicompat.Name,
		BaseURL: server.URL,
		APIKey:  "test-api-key",
		Models: []catwalk.Model{{
			ID:               smallCfg.Model,
			Name:             "Small Test Model",
			ContextWindow:    64_000,
			DefaultMaxTokens: 2_048,
		}},
	})

	coord := &coordinator{cfg: cfg}
	enhanced, err := coord.EnhancePrompt(t.Context(), "", "你是谁")
	require.NoError(t, err)
	require.Equal(t, "增强结果", enhanced)
}

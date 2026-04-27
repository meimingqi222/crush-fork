package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type promptEnhanceLanguageModel struct {
	provider string
	model    string
}

func (m promptEnhanceLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	panic("unexpected Generate call")
}

func (m promptEnhanceLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
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

func TestEnhancePromptModelPrefersCurrentAgentModel(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{ID: "test-provider"}
	cfg.Config().Providers.Set("test-provider", providerCfg)

	want := Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 64_000, DefaultMaxTokens: 2_048},
		ModelCfg:   config.SelectedModel{Provider: "test-provider", Model: "current-model"},
		Model:      promptEnhanceLanguageModel{provider: "test-provider", model: "current-model"},
	}
	coord := &coordinator{
		cfg: cfg,
		currentAgent: &mockSessionAgent{
			model: want,
		},
	}

	got, gotProvider, err := coord.enhancePromptModel(t.Context())
	require.NoError(t, err)
	require.Equal(t, want.ModelCfg.Model, got.ModelCfg.Model)
	require.Equal(t, want.ModelCfg.Provider, got.ModelCfg.Provider)
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

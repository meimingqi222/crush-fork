package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestAgentModelInfoCarriesEffectiveContextWindow(t *testing.T) {
	t.Parallel()
	info := agentModelInfo(Model{
		ModelCfg: config.SelectedModel{Provider: "openai", Model: "gpt-4"},
		CatwalkCfg: catwalk.Model{
			ContextWindow: 400_000,
			Options: catwalk.ModelOptions{
				ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
			},
		},
	})
	require.Equal(t, "openai", info.ProviderID)
	require.Equal(t, "gpt-4", info.ModelID)
	require.Equal(t, int64(400_000), info.ContextWindow)
	require.Equal(t, int64(150_000), info.MaxPromptTokens)
	require.Equal(t, int64(150_000), info.EffectiveContextWindow)
}

func TestUsageSnapshotFromMessagesReturnsLatestAssistantUsage(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		{Role: message.User},
		{Role: message.Assistant, Usage: message.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50}},
		{Role: message.User},
		{Role: message.Assistant, Usage: message.Usage{InputTokens: 3000, OutputTokens: 500, CacheReadTokens: 100, ReasoningTokens: 40}},
	}
	snap := usageSnapshotFromMessages(msgs, 0)
	require.Equal(t, int64(3000+100), snap.PromptTokens)
	require.Equal(t, int64(540), snap.CompletionTokens)
	require.Equal(t, int64(40), snap.ReasoningTokens)
	require.Equal(t, int64(100), snap.CacheReadTokens)
	require.Equal(t, int64(3640), snap.TotalTokens)
	require.Equal(t, int64(3640), snap.ContextUsed)
}

func TestUsageSnapshotFromMessagesContextUsedUsesEstimateWhenLarger(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		{Role: message.Assistant, Usage: message.Usage{InputTokens: 1000, OutputTokens: 200}},
	}
	snap := usageSnapshotFromMessages(msgs, 5000)
	require.Equal(t, int64(5000), snap.EstimatedPromptTokens)
	require.Equal(t, int64(5000), snap.ContextUsed)
	require.Equal(t, int64(1200), snap.TotalTokens)
}

func TestUsageSnapshotFromMessagesNoAssistant(t *testing.T) {
	t.Parallel()
	snap := usageSnapshotFromMessages(nil, 1234)
	require.Equal(t, int64(0), snap.TotalTokens)
	require.Equal(t, int64(1234), snap.EstimatedPromptTokens)
	require.Equal(t, int64(1234), snap.ContextUsed)
}

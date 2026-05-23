package model

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestLatestAssistantUsageSnapshotUsesLastAssistantUsageModel(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	cfg.Providers.Set("anthropic", config.ProviderConfig{
		ID: "anthropic",
		Models: []catwalk.Model{{
			ID:            "claude-sonnet-4-5",
			ContextWindow: 200_000,
		}},
	})

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 400_000},
		ModelCfg:   config.SelectedModel{Provider: "openai", Model: "gpt-5"},
	}

	snapshot, ok := latestAssistantUsageSnapshot([]message.Message{
		{
			Role:     message.Assistant,
			Provider: "openai",
			Model:    "gpt-5",
			Usage: message.Usage{
				InputTokens:  1000,
				OutputTokens: 200,
			},
		},
		{
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Usage: message.Usage{
				InputTokens:      120,
				OutputTokens:     45,
				ReasoningTokens:  30,
				CacheReadTokens:  900,
				CacheWriteTokens: 300,
			},
		},
	}, cfg, selected)
	require.True(t, ok)
	// TotalTokens = PromptTokens() + OutputTokens = (120 + 900 + 300) + 45 = 1365
	// (OutputTokens already includes ReasoningTokens for OpenAI-style providers)
	require.Equal(t, int64(1365), snapshot.TotalTokens)
	require.Equal(t, int64(45), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
}

func TestLatestAssistantUsageSnapshotReturnsLastAssistantWithPositiveTokens(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow: 200_000,
			Options: catwalk.ModelOptions{
				ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
			},
		},
		ModelCfg: config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	snapshot, ok := latestAssistantUsageSnapshot([]message.Message{
		{
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Usage: message.Usage{
				InputTokens:      100,
				OutputTokens:     40,
				ReasoningTokens:  20,
				CacheReadTokens:  300,
				CacheWriteTokens: 200,
			},
		},
		{
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Usage: message.Usage{
				InputTokens:      10,
				OutputTokens:     0,
				CacheReadTokens:  50,
				CacheWriteTokens: 25,
			},
		},
	}, nil, selected)
	require.True(t, ok)
	require.Equal(t, int64(85), snapshot.TotalTokens)
	require.Equal(t, int64(0), snapshot.OutputTokens)
	require.Equal(t, int64(150_000), snapshot.ContextWindow)
}

func TestResolveContextUsageSnapshotReturnsProvisionalDirectly(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	snapshot := resolveContextUsageSnapshot(&session.Session{
		LastPromptTokens:     110_000,
		LastCompletionTokens: 600,
		CompletionTokens:     600,
	}, []message.Message{
		{
			ID:       "finished",
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Parts: []message.ContentPart{
				message.TextContent{Text: "done"},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: 1},
			},
			Usage: message.Usage{
				InputTokens:      100_000,
				OutputTokens:     600,
				CacheReadTokens:  8_000,
				CacheWriteTokens: 2_000,
			},
		},
		{
			ID:       "current",
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Usage: message.Usage{
				InputTokens:      2_500,
				OutputTokens:     20,
				CacheReadTokens:  500,
				CacheWriteTokens: 250,
			},
		},
	}, nil, selected)

	require.Equal(t, int64(110_600), snapshot.TotalTokens)
	require.Equal(t, int64(600), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
	require.True(t, snapshot.Provisional)
	require.False(t, snapshot.Summary)
}

func TestDisplayContextWindowUsesMinOfWindowAndMaxPromptTokens(t *testing.T) {
	t.Parallel()

	window := agent.EffectiveContextWindow(catwalk.Model{
		ContextWindow: 400_000,
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
		},
	})

	require.Equal(t, int64(150_000), window)
}

func TestDisplayContextWindowFallsBackToMaxPromptTokens(t *testing.T) {
	t.Parallel()

	window := agent.EffectiveContextWindow(catwalk.Model{
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
		},
	})

	require.Equal(t, int64(150_000), window)
}

func TestResolveContextUsageSnapshotFloorsProvisionalToSessionHistory(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	// Only a provisional assistant message exists and no finished message
	// is available. The snapshot must be floored to the session's last
	// confirmed totals so the display does not drop below known history.
	snapshot := resolveContextUsageSnapshot(&session.Session{
		LastPromptTokens:     50_000,
		LastCompletionTokens: 500,
		CompletionTokens:     500,
	}, []message.Message{
		{
			ID:       "current",
			Role:     message.Assistant,
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Usage: message.Usage{
				InputTokens:      0,
				OutputTokens:     1_200,
				CacheReadTokens:  0,
				CacheWriteTokens: 0,
			},
		},
	}, nil, selected)

	require.Equal(t, int64(50_500), snapshot.TotalTokens)
	require.Equal(t, int64(1_200), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
	require.True(t, snapshot.Provisional)
	require.False(t, snapshot.Summary)
}

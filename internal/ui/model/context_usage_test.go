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
	require.Equal(t, int64(1395), snapshot.TotalTokens)
	require.Equal(t, int64(75), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
}

func TestLatestAssistantUsageSnapshotSkipsZeroOutputMessagesAndUsesSelectedContextWindow(t *testing.T) {
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
	require.Equal(t, int64(660), snapshot.TotalTokens)
	require.Equal(t, int64(60), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
}

func TestApplySessionUsageFloorUsesHigherSessionUsage(t *testing.T) {
	t.Parallel()

	snapshot := applySessionUsageFloor(contextUsageSnapshot{
		TotalTokens:   295,
		OutputTokens:  45,
		ContextWindow: 200_000,
	}, &session.Session{
		LastPromptTokens:     18_500,
		LastCompletionTokens: 200,
	})

	require.Equal(t, int64(18_700), snapshot.TotalTokens)
	require.Equal(t, int64(200), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
}

func TestApplySessionUsageFloorSkipsProvisionalSnapshot(t *testing.T) {
	t.Parallel()

	snapshot := applySessionUsageFloor(contextUsageSnapshot{
		TotalTokens:   1_200,
		OutputTokens:  0,
		ContextWindow: 200_000,
		Provisional:   true,
	}, &session.Session{
		LastPromptTokens:     18_500,
		LastCompletionTokens: 200,
	})

	require.Equal(t, int64(1_200), snapshot.TotalTokens)
	require.Equal(t, int64(0), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
	require.True(t, snapshot.Provisional)
}

func TestResolveContextUsageSnapshotPrefersFinishedAssistantOverSmallerProvisional(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	snapshot := resolveContextUsageSnapshot(&session.Session{
		LastPromptTokens:     110_000,
		LastCompletionTokens: 600,
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
	require.False(t, snapshot.Provisional)
	require.False(t, snapshot.Summary)
}

func TestDisplayContextWindowFallsBackToMaxPromptTokens(t *testing.T) {
	t.Parallel()

	window := displayContextWindow(agent.Model{
		CatwalkCfg: catwalk.Model{
			Options: catwalk.ModelOptions{
				ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
			},
		},
	})

	require.Equal(t, int64(150_000), window)
}

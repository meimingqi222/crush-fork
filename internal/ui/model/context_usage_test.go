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
		Models: []config.ProviderModel{{
			Model: catwalk.Model{
				ID:            "claude-sonnet-4-5",
				ContextWindow: 200_000,
			},
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
		PromptTokens:         110_000,
		CompletionTokens:     600,
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

	// For provisional messages the current exchange has not been committed
	// to session totals yet. TotalTokens is anchored at the last known
	// exchange total (110_000 + 600 = 110_600) plus the streaming output
	// estimate (20 tokens from the provisional usage). OutputTokens is the
	// committed cumulative total (600) plus the streaming estimate (20).
	require.Equal(t, int64(110_620), snapshot.TotalTokens)
	require.Equal(t, int64(620), snapshot.OutputTokens)
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

// TestLatestAssistantUsageSnapshotIgnoresSummaryPromptTokens is a Phase B/D
// invariant test for docs/refactor-context-usage-accounting.md: a streaming
// summary (compaction) message reports its OWN prompt tokens, which reflect
// the pre-compaction history, not the current context. Before the fix these
// were added to OutputTokens, making the context-usage display jump above
// 100% while/after compacting. Only the summary's output length is
// meaningful here.
func TestLatestAssistantUsageSnapshotIgnoresSummaryPromptTokens(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	snapshot, ok := latestAssistantUsageSnapshot([]message.Message{
		{
			ID:               "summary",
			Role:             message.Assistant,
			Provider:         "anthropic",
			Model:            "claude-sonnet-4-5",
			IsSummaryMessage: true,
			// A huge pre-compaction prompt, as a real summarize call sends.
			Usage: message.Usage{
				InputTokens:  190_000,
				OutputTokens: 500,
			},
			// No Finish part: the summary is still streaming.
		},
	}, nil, selected)

	require.True(t, ok)
	require.Equal(t, int64(500), snapshot.TotalTokens,
		"summary snapshot TotalTokens must be the output length only, not prompt+output")
	require.Equal(t, int64(500), snapshot.OutputTokens)
	require.True(t, snapshot.Provisional)
	require.True(t, snapshot.Summary)
}

// TestResolveContextUsageSnapshotSummaryStreamingStaysWithinContextWindow is
// a Phase D UI snapshot test: while a summary message streams, the resolved
// context-usage snapshot must not report TotalTokens/ContextWindow above
// 1.0, even though the summary's own (pre-compaction) prompt tokens are
// close to the context window and its growing output alone would not be.
// This reproduces the "jumps above 100% after compacting" bug scenario from
// docs/refactor-context-usage-accounting.md.
func TestResolveContextUsageSnapshotSummaryStreamingStaysWithinContextWindow(t *testing.T) {
	t.Parallel()

	const contextWindow = 200_000
	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: contextWindow},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	// The session's last known (pre-compaction) exchange was near the
	// auto-summarize threshold.
	sess := &session.Session{
		PromptTokens:         190_000,
		CompletionTokens:     1_000,
		LastPromptTokens:     190_000,
		LastCompletionTokens: 1_000,
	}

	snapshot := resolveContextUsageSnapshot(sess, []message.Message{
		{
			ID:               "summary",
			Role:             message.Assistant,
			Provider:         "anthropic",
			Model:            "claude-sonnet-4-5",
			IsSummaryMessage: true,
			Usage: message.Usage{
				// The summarize call's own prompt tokens: pre-compaction
				// history, close to the context window.
				InputTokens: 190_000,
				// A large, still-growing summary.
				OutputTokens: 15_000,
			},
			// Not finished: the summary is still streaming.
		},
	}, nil, selected)

	require.True(t, snapshot.Provisional)
	require.True(t, snapshot.Summary)
	require.LessOrEqual(t, float64(snapshot.TotalTokens)/float64(snapshot.ContextWindow), 1.0,
		"context usage must not exceed 100% while a summary message is streaming")
}

// TestSiblingPositionFindsIndexAndCount covers the pure lookup used by
// refreshSiblingIndex to compute the subagent footer's "(n of N)" position.
// The event-driven caching around it (refresh on session switch and on
// session pubsub events) requires DB/pubsub mocks and is intentionally not
// covered here.
func TestSiblingPositionFindsIndexAndCount(t *testing.T) {
	t.Parallel()

	children := []session.Session{
		{ID: "child-1"},
		{ID: "child-2"},
		{ID: "child-3"},
	}

	index, count := siblingPosition(children, "child-2")
	require.Equal(t, 2, index)
	require.Equal(t, 3, count)
}

func TestSiblingPositionReturnsZeroIndexWhenCurrentNotFound(t *testing.T) {
	t.Parallel()

	children := []session.Session{
		{ID: "child-1"},
		{ID: "child-2"},
	}

	// Simulates a stale view where the current session ID no longer
	// appears among its parent's children.
	index, count := siblingPosition(children, "missing-child")
	require.Equal(t, 0, index)
	require.Equal(t, 2, count)
}

func TestSiblingPositionEmptyChildren(t *testing.T) {
	t.Parallel()

	index, count := siblingPosition(nil, "child-1")
	require.Equal(t, 0, index)
	require.Equal(t, 0, count)
}

func TestResolveContextUsageSnapshotAccumulatesProvisionalExchange(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	// Only a provisional assistant message exists and no finished message
	// is available. The snapshot should be floored at the last known
	// exchange total so the display never drops below the confirmed context
	// length.
	snapshot := resolveContextUsageSnapshot(&session.Session{
		PromptTokens:         50_000,
		CompletionTokens:     500,
		LastPromptTokens:     50_000,
		LastCompletionTokens: 500,
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

	// TotalTokens is anchored at the last exchange total (50_000 + 500 =
	// 50_500) plus the streaming output (1_200 from the provisional usage).
	// OutputTokens is the committed cumulative total (500) plus the
	// streaming output (1_200).
	require.Equal(t, int64(51_700), snapshot.TotalTokens)
	require.Equal(t, int64(1_700), snapshot.OutputTokens)
	require.Equal(t, int64(200_000), snapshot.ContextWindow)
	require.True(t, snapshot.Provisional)
	require.False(t, snapshot.Summary)
}

// TestProvisionalTotalNeverExceedsFinishedTotal verifies the core fix for
// the "context usage suddenly drops" bug: during streaming, the provisional
// TotalTokens (anchored at lastTotal + streamingOutput) must never exceed
// the finished TotalTokens (realPrompt + realOutput) that replaces it.
func TestProvisionalTotalNeverExceedsFinishedTotal(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "openai", Model: "gpt-5"},
	}

	// Simulate a session where the last exchange used 100k prompt + 2k output.
	// The next exchange's estimated prompt (set by agent.go) is 130k, which
	// overshoots the real 105k by ~24%. Before the fix, the provisional
	// display would show max(130k, 102k) = 130k, then drop to 105k + 3k = 108k
	// when the step finishes.
	sess := &session.Session{
		PromptTokens:         100_000,
		CompletionTokens:     2_000,
		LastPromptTokens:     100_000,
		LastCompletionTokens: 2_000,
	}

	// Provisional: assistant message with an inflated prompt estimate and
	// some streamed text (~20 tokens).
	provisionalSnapshot := resolveContextUsageSnapshot(sess, []message.Message{
		{
			ID:       "current",
			Role:     message.Assistant,
			Provider: "openai",
			Model:    "gpt-5",
			Parts: []message.ContentPart{
				message.TextContent{Text: "Here is a brief reply."},
			},
			Usage: message.Usage{
				InputTokens: 130_000, // over-estimated by ~24%
			},
		},
	}, nil, selected)

	// Finished: the session now has the real prompt (105k) and output (3k).
	finishedSess := &session.Session{
		PromptTokens:         105_000,
		CompletionTokens:     5_000, // cumulative: 2k + 3k
		LastPromptTokens:     105_000,
		LastCompletionTokens: 3_000,
	}
	finishedSnapshot := resolveContextUsageSnapshot(finishedSess, []message.Message{
		{
			ID:       "current",
			Role:     message.Assistant,
			Provider: "openai",
			Model:    "gpt-5",
			Parts: []message.ContentPart{
				message.TextContent{Text: "Here is a brief reply."},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: 1},
			},
			Usage: message.Usage{
				InputTokens:  105_000,
				OutputTokens: 3_000,
			},
		},
	}, nil, selected)

	require.True(t, provisionalSnapshot.Provisional)
	require.False(t, finishedSnapshot.Provisional)
	require.LessOrEqual(t, provisionalSnapshot.TotalTokens, finishedSnapshot.TotalTokens,
		"provisional TotalTokens (%d) must not exceed finished TotalTokens (%d)",
		provisionalSnapshot.TotalTokens, finishedSnapshot.TotalTokens)
	require.LessOrEqual(t, provisionalSnapshot.OutputTokens, finishedSnapshot.OutputTokens,
		"provisional OutputTokens (%d) must not exceed finished OutputTokens (%d)",
		provisionalSnapshot.OutputTokens, finishedSnapshot.OutputTokens)
}

// TestProvisionalAfterCompactionUsesPostCompactionBaseline verifies that
// after a context compaction, the provisional (streaming) display for the
// next exchange correctly anchors at the post-compaction LastTotalTokens
// rather than the pre-compaction values. This is the critical compaction
// boundary scenario:
//
//  1. Before compaction: session had ~190k tokens (near auto-summarize).
//  2. Compaction runs: Summarize recomputes LastPromptTokens to the
//     post-compaction baseline (e.g. 20k), LastCompletionTokens to 0.
//     CompletionTokens keeps accumulating (includes summary output).
//  3. Post-compaction streaming: the new exchange must display
//     lastTotal + streamingOutput, NOT the pre-compaction 190k.
//  4. Post-compaction finished: display = LastTotalTokens() (the real
//     post-compaction prompt + output).
//
// The provisional value must never exceed the finished value.
func TestProvisionalAfterCompactionUsesPostCompactionBaseline(t *testing.T) {
	t.Parallel()

	selected := &agent.Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200_000},
		ModelCfg:   config.SelectedModel{Provider: "openai", Model: "gpt-5"},
	}

	// Post-compaction session state: Summarize has reset
	// LastPromptTokens to the post-compaction baseline and
	// LastCompletionTokens to 0. CompletionTokens is cumulative
	// (includes all prior output + the summary's own output).
	postCompactionSess := &session.Session{
		PromptTokens:         20_000, // post-compaction baseline
		CompletionTokens:     25_000, // cumulative (pre-compaction + summary)
		LastPromptTokens:     20_000, // post-compaction baseline
		LastCompletionTokens: 0,      // reset by Summarize
	}

	// Provisional: new exchange streaming after compaction.
	// The message's InputTokens is a local estimate that may be
	// inflated, but our code must ignore it and use lastTotal.
	provisionalSnapshot := resolveContextUsageSnapshot(postCompactionSess, []message.Message{
		{
			ID:       "post-compaction-reply",
			Role:     message.Assistant,
			Provider: "openai",
			Model:    "gpt-5",
			Parts: []message.ContentPart{
				message.TextContent{Text: "Here is a reply after compaction."},
			},
			Usage: message.Usage{
				InputTokens: 50_000, // local estimate, should be ignored
			},
		},
	}, nil, selected)

	// Finished: the real API usage arrives. The prompt is the
	// post-compaction context (summary + tail + new user msg).
	finishedSess := &session.Session{
		PromptTokens:         22_000,
		CompletionTokens:     25_500, // 25k + 500 new output
		LastPromptTokens:     22_000,
		LastCompletionTokens: 500,
	}
	finishedSnapshot := resolveContextUsageSnapshot(finishedSess, []message.Message{
		{
			ID:       "post-compaction-reply",
			Role:     message.Assistant,
			Provider: "openai",
			Model:    "gpt-5",
			Parts: []message.ContentPart{
				message.TextContent{Text: "Here is a reply after compaction."},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: 1},
			},
			Usage: message.Usage{
				InputTokens:  22_000,
				OutputTokens: 500,
			},
		},
	}, nil, selected)

	require.True(t, provisionalSnapshot.Provisional)
	require.False(t, provisionalSnapshot.Summary,
		"post-compaction exchange must not be flagged as summary")
	require.False(t, finishedSnapshot.Provisional)

	// Provisional TotalTokens must be anchored at post-compaction
	// lastTotal (20k) + streamingOutput, NOT the inflated 50k estimate.
	require.Less(t, provisionalSnapshot.TotalTokens, int64(50_000),
		"provisional TotalTokens must not use the inflated local prompt estimate")

	// Provisional must not exceed finished (no drop on completion).
	require.LessOrEqual(t, provisionalSnapshot.TotalTokens, finishedSnapshot.TotalTokens,
		"provisional TotalTokens (%d) must not exceed finished TotalTokens (%d) after compaction",
		provisionalSnapshot.TotalTokens, finishedSnapshot.TotalTokens)
	require.LessOrEqual(t, provisionalSnapshot.OutputTokens, finishedSnapshot.OutputTokens,
		"provisional OutputTokens (%d) must not exceed finished OutputTokens (%d) after compaction",
		provisionalSnapshot.OutputTokens, finishedSnapshot.OutputTokens)
}

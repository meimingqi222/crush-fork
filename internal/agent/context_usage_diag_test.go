package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestContextUsageDiagEnabled(t *testing.T) {
	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "")
	require.False(t, contextUsageDiagEnabled())

	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "1")
	require.True(t, contextUsageDiagEnabled())
}

func TestLogContextUsageDiagnosticSkipsWhenDisabled(t *testing.T) {
	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "")
	buf := captureSlogOutput(t, slog.LevelInfo)
	logContextUsageDiagnostic(contextUsageDiagnosticInput{
		SessionID: "sess-1",
		Model: Model{
			ModelCfg: config.SelectedModel{Model: "grok-composer-2.5-fast", Provider: "copilot"},
		},
	})
	require.Empty(t, buf.String())
}

func TestLogContextUsageDiagnosticEmitsWhenEnabled(t *testing.T) {
	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "1")
	buf := captureSlogOutput(t, slog.LevelInfo)

	normalized := normalizedMessageUsage(fantasy.Usage{
		InputTokens:  95,
		OutputTokens: 200,
	}, "anthropic", 18_500)

	logContextUsageDiagnostic(contextUsageDiagnosticInput{
		SessionID: "sess-1",
		Model: Model{
			ModelCfg: config.SelectedModel{Model: "claude-sonnet-4-5", Provider: "anthropic"},
		},
		ProviderUsage: fantasy.Usage{
			InputTokens:  95,
			OutputTokens: 200,
		},
		NormalizedUsage:       normalized,
		EstimatedPromptTokens: 18_500,
		PreparedMessageCount:  18,
	})

	out := buf.String()
	require.Contains(t, out, "Context usage diagnostic")
	require.Contains(t, out, "display_total_tokens")
	require.Contains(t, out, "estimate_floor")
}

func captureSlogOutput(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})
	return &buf
}

func TestLogContextUsageDiagnosticJSONFields(t *testing.T) {
	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "1")
	buf := captureSlogOutput(t, slog.LevelInfo)

	logContextUsageDiagnostic(contextUsageDiagnosticInput{
		SessionID: "sess-2",
		Model: Model{
			ModelCfg: config.SelectedModel{Model: "grok-composer-2.5-fast", Provider: "openai"},
		},
		ProviderUsage: fantasy.Usage{
			InputTokens:  12_000,
			OutputTokens: 800,
		},
		NormalizedUsage: message.Usage{
			InputTokens:  12_000,
			OutputTokens: 800,
		},
		UsageEstimated:       true,
		PreparedMessageCount: 6,
	})

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.NotEmpty(t, lines)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &payload))
	require.Equal(t, "Context usage diagnostic", payload["msg"])
	require.Equal(t, "fallback_estimate", payload["usage_source"])
	require.EqualValues(t, 12800, payload["display_total_tokens"])
}

func TestLogContextUsageDiagnosticSegmentBreakdown(t *testing.T) {
	t.Setenv("CRUSH_CONTEXT_USAGE_DIAG", "1")
	buf := captureSlogOutput(t, slog.LevelInfo)

	logContextUsageDiagnostic(contextUsageDiagnosticInput{
		SessionID: "sess-3",
		Model: Model{
			ModelCfg: config.SelectedModel{Model: "claude-sonnet-4-5", Provider: "anthropic"},
		},
		ProviderUsage: fantasy.Usage{
			InputTokens:  10_000,
			OutputTokens: 500,
		},
		NormalizedUsage: message.Usage{
			InputTokens:  10_000,
			OutputTokens: 500,
		},
		PreparedMessageCount: 5,
		// Segment breakdown.
		SystemPromptTokens: 8_000,
		ToolSchemaTokens:   3_000,
		PriorHistoryTokens: 12_000,
		CurrentUserTokens:  50,
		PromptPrefixTokens: 100,
		PromptSuffixTokens: 20,
		ContextFilesTokens: 2_000,
		ContextFilesCount:  3,
	})

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.NotEmpty(t, lines)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &payload))
	require.Equal(t, "Context usage diagnostic", payload["msg"])
	require.EqualValues(t, 8000, payload["segment_system_prompt_tokens"])
	require.EqualValues(t, 3000, payload["segment_tool_schema_tokens"])
	require.EqualValues(t, 12000, payload["segment_prior_history_tokens"])
	require.EqualValues(t, 50, payload["segment_current_user_tokens"])
	require.EqualValues(t, 100, payload["segment_prompt_prefix_tokens"])
	require.EqualValues(t, 20, payload["segment_prompt_suffix_tokens"])
	require.EqualValues(t, 2000, payload["segment_context_files_tokens"])
	require.EqualValues(t, 3, payload["segment_context_files_count"])
}

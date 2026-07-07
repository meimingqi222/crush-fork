package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

func TestBuildSummaryPromptIncludesTrackedTasksWithoutTodoInstructions(t *testing.T) {
	t.Parallel()

	prompt := buildSummaryPrompt([]session.Todo{{
		Status:  session.TodoStatusInProgress,
		Content: "Investigate delegation bias",
	}})

	require.Contains(t, prompt, "## Tracked Tasks")
	require.Contains(t, prompt, "[in_progress] Investigate delegation bias")
	require.NotContains(t, prompt, "use the `todos` tool")
}

func TestPromptTokensForUsage_OpenAIStyle(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:     120,
		CacheReadTokens: 900,
		OutputTokens:    45,
	}

	// All providers built on the OpenAI SDK normalize InputTokens by
	// subtracting CacheReadTokens, so promptTokensForUsage must add them back.
	for _, providerID := range []string{"openai", "azure", "openai-compat"} {
		t.Run(providerID, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, int64(1020), promptTokensForUsage(usage, providerID))
			require.Equal(t, int64(1065), totalTokensForUsage(usage, providerID))
		})
	}
}

func TestPromptTokensForUsage_AnthropicCacheStyle(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:         120,
		CacheCreationTokens: 300,
		CacheReadTokens:     900,
		OutputTokens:        45,
	}

	// Anthropic-style: InputTokens does NOT include cached tokens
	require.Equal(t, int64(1320), promptTokensForUsage(usage, "anthropic"))
	require.Equal(t, int64(1365), totalTokensForUsage(usage, "anthropic"))
	require.Equal(t, int64(1365), totalTokensForUsage(usage, "@ai-sdk/anthropic"))
	require.Equal(t, int64(1365), totalTokensForUsage(usage, "@ai-sdk/google-vertex/anthropic"))

	// OpenAI-style in fantasy/providers/openai: InputTokens excludes cached
	// prompt reuse, so CacheReadTokens must be added back for display.
	require.Equal(t, int64(1320), promptTokensForUsage(usage, "openai"))
}

func TestNormalizedMessageUsage_PreservesCacheBreakdown(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:         120,
		CacheCreationTokens: 300,
		CacheReadTokens:     900,
		OutputTokens:        45,
		ReasoningTokens:     20,
	}

	normalized := normalizedMessageUsage(usage, "openai", 0)

	require.Equal(t, message.Usage{
		InputTokens:      120,
		OutputTokens:     45,
		ReasoningTokens:  20,
		CacheReadTokens:  900,
		CacheWriteTokens: 300,
	}, normalized)
	require.Equal(t, int64(1320), normalized.PromptTokens())
	require.Equal(t, int64(1385), normalized.TotalTokens())
}

func TestNormalizedMessageUsage_PrefersEstimatedPromptFloor(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:  95,
		OutputTokens: 200,
	}

	normalized := normalizedMessageUsage(usage, "anthropic", 18_500)

	require.Equal(t, int64(18_500), normalized.InputTokens)
	require.Equal(t, int64(200), normalized.OutputTokens)
	require.Equal(t, int64(18_700), normalized.TotalTokens())
}

func TestNormalizedMessageUsage_TrustsOpenAIProviderUsageOverEstimate(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:     1,
		OutputTokens:    85,
		CacheReadTokens: 36_381,
	}

	normalized := normalizedMessageUsage(usage, "openai", 54_770)

	require.Equal(t, int64(1), normalized.InputTokens)
	require.Equal(t, int64(36_381), normalized.CacheReadTokens)
	require.Equal(t, int64(36_382), normalized.PromptTokens())
	require.Equal(t, int64(36_467), normalized.TotalTokens())
}

func TestNormalizedMessageUsage_FloorsToEstimateWhenProviderOmitsPrompt(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		OutputTokens: 200,
	}

	normalized := normalizedMessageUsage(usage, "openai", 12_000)

	require.Equal(t, int64(12_000), normalized.InputTokens)
	require.Equal(t, int64(12_200), normalized.TotalTokens())
}

func TestShouldFloorPromptTokensToEstimate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		usage                 fantasy.Usage
		providerID            string
		promptTokens          int64
		estimatedPromptTokens int64
		want                  bool
	}{
		{
			name:                  "no estimate",
			usage:                 fantasy.Usage{InputTokens: 10},
			providerID:            "anthropic",
			promptTokens:          10,
			estimatedPromptTokens: 0,
			want:                  false,
		},
		{
			name:                  "provider already at or above estimate",
			usage:                 fantasy.Usage{InputTokens: 20_000},
			providerID:            "anthropic",
			promptTokens:          20_000,
			estimatedPromptTokens: 18_500,
			want:                  false,
		},
		{
			name:                  "provider omits prompt tokens",
			usage:                 fantasy.Usage{OutputTokens: 42},
			providerID:            "anthropic",
			promptTokens:          0,
			estimatedPromptTokens: 18_500,
			want:                  true,
		},
		{
			name: "anthropic under-report without cache",
			usage: fantasy.Usage{
				InputTokens:  95,
				OutputTokens: 200,
			},
			providerID:            "anthropic",
			promptTokens:          95,
			estimatedPromptTokens: 18_500,
			want:                  true,
		},
		{
			name: "cache read blocks floor for anthropic",
			usage: fantasy.Usage{
				InputTokens:     1,
				CacheReadTokens: 36_381,
			},
			providerID:            "anthropic",
			promptTokens:          36_382,
			estimatedPromptTokens: 54_770,
			want:                  false,
		},
		{
			name: "cache creation blocks floor for anthropic",
			usage: fantasy.Usage{
				InputTokens:         120,
				CacheCreationTokens: 300,
			},
			providerID:            "anthropic",
			promptTokens:          420,
			estimatedPromptTokens: 18_500,
			want:                  false,
		},
		{
			name: "openai provider trusts reported usage",
			usage: fantasy.Usage{
				InputTokens:     1,
				CacheReadTokens: 36_381,
			},
			providerID:            "openai",
			promptTokens:          36_382,
			estimatedPromptTokens: 54_770,
			want:                  false,
		},
		{
			name: "openai-compat provider trusts reported usage",
			usage: fantasy.Usage{
				InputTokens:  12_000,
				OutputTokens: 200,
			},
			providerID:            "openai-compat",
			promptTokens:          12_000,
			estimatedPromptTokens: 18_500,
			want:                  false,
		},
		{
			name: "openai provider floors when prompt omitted",
			usage: fantasy.Usage{
				OutputTokens: 200,
			},
			providerID:            "openai",
			promptTokens:          0,
			estimatedPromptTokens: 12_000,
			want:                  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldFloorPromptTokensToEstimate(
				tt.usage,
				tt.providerID,
				tt.promptTokens,
				tt.estimatedPromptTokens,
			)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestShouldAutoSummarize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		model           Model
		contextUsed     int64
		maxOutputTokens int64
		want            bool
	}{
		{
			name: "fallback context budget reserves bounded buffer instead of full max output",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    200_000,
				DefaultMaxTokens: 50_000,
			}},
			contextUsed:     180_000,
			maxOutputTokens: 50_000,
			want:            true,
		},
		{
			name: "fallback context budget stays below reserved buffer threshold",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    200_000,
				DefaultMaxTokens: 50_000,
			}},
			contextUsed:     179_999,
			maxOutputTokens: 50_000,
			want:            false,
		},
		{
			name: "large default max output does not force premature summarize on 200k models",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    204_800,
				DefaultMaxTokens: 131_072,
			}},
			contextUsed:     60_000,
			maxOutputTokens: 0,
			want:            false,
		},
		{
			name: "anthropic 200k model does not summarize at 140k because max output is large",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    200_000,
				DefaultMaxTokens: 64_000,
			}},
			contextUsed:     140_000,
			maxOutputTokens: 64_000,
			want:            false,
		},
		{
			name: "explicit max prompt tokens uses reserved buffer instead of full output reservation",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    400_000,
				DefaultMaxTokens: 128_000,
				Options: catwalk.ModelOptions{
					ProviderOptions: map[string]any{"max_prompt_tokens": 272_000},
				},
			}},
			contextUsed:     252_000,
			maxOutputTokens: 128_000,
			want:            true,
		},
		{
			name: "explicit max prompt tokens leaves room below reserved buffer threshold",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    400_000,
				DefaultMaxTokens: 128_000,
				Options: catwalk.ModelOptions{
					ProviderOptions: map[string]any{"max_prompt_tokens": 272_000},
				},
			}},
			contextUsed:     251_999,
			maxOutputTokens: 128_000,
			want:            false,
		},
		{
			name: "uses model default max output when request does not specify one",
			model: Model{CatwalkCfg: catwalk.Model{
				ContextWindow:    100_000,
				DefaultMaxTokens: 8_000,
			}},
			contextUsed:     92_000,
			maxOutputTokens: 0,
			want:            true,
		},
		{
			name:            "invalid context window",
			model:           Model{CatwalkCfg: catwalk.Model{}},
			contextUsed:     1,
			maxOutputTokens: 8_000,
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, shouldAutoSummarize(tt.model, tt.contextUsed, tt.maxOutputTokens))
		})
	}
}

func TestRefreshCallConfigIfNeeded_UsesContextRuntimeConfig(t *testing.T) {
	t.Parallel()

	call := SessionAgentCall{MaxOutputTokens: 1}
	runtimeConfig := sessionAgentRuntimeConfig{MaxOutputTokens: 2048}
	agent := &sessionAgent{
		refreshCallConfig: func(context.Context) (sessionAgentRuntimeConfig, error) {
			return sessionAgentRuntimeConfig{MaxOutputTokens: 4096}, nil
		},
	}

	ctx := context.WithValue(context.Background(), sessionAgentRuntimeConfigContextKey{}, runtimeConfig)
	runtimeConfigPtr, err := agent.refreshCallConfigIfNeeded(ctx, &call)
	require.NoError(t, err)
	require.NotNil(t, runtimeConfigPtr)
	require.Equal(t, int64(2048), call.MaxOutputTokens)
}

func TestRefreshCallConfigIfNeeded_IgnoresNilPointerContextAndRefreshes(t *testing.T) {
	t.Parallel()

	call := SessionAgentCall{MaxOutputTokens: 1}
	agent := &sessionAgent{
		refreshCallConfig: func(context.Context) (sessionAgentRuntimeConfig, error) {
			return sessionAgentRuntimeConfig{MaxOutputTokens: 4096}, nil
		},
	}

	ctx := context.WithValue(context.Background(), sessionAgentRuntimeConfigContextKey{}, (*sessionAgentRuntimeConfig)(nil))
	runtimeConfigPtr, err := agent.refreshCallConfigIfNeeded(ctx, &call)
	require.NoError(t, err)
	require.NotNil(t, runtimeConfigPtr)
	require.Equal(t, int64(4096), call.MaxOutputTokens)
}

func float64Ptr(v float64) *float64 {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

func ptrAddress[T any](p *T) uintptr {
	if p == nil {
		return 0
	}
	return uintptr(unsafe.Pointer(p))
}

func TestApplyRuntimeConfig_OnlyOverridesExplicitPointerFields(t *testing.T) {
	t.Parallel()

	temperature := 0.7
	topP := 0.9
	topK := int64(50)
	frequencyPenalty := 0.2
	presencePenalty := 0.3

	call := SessionAgentCall{
		MaxOutputTokens:  256,
		Temperature:      float64Ptr(0.1),
		TopP:             float64Ptr(0.2),
		TopK:             int64Ptr(3),
		FrequencyPenalty: float64Ptr(-0.1),
		PresencePenalty:  float64Ptr(-0.2),
	}

	applyRuntimeConfig(&call, sessionAgentRuntimeConfig{
		MaxOutputTokens:  1024,
		Temperature:      &temperature,
		TopP:             &topP,
		TopK:             &topK,
		FrequencyPenalty: &frequencyPenalty,
		PresencePenalty:  &presencePenalty,
	})

	require.Equal(t, int64(1024), call.MaxOutputTokens)
	require.Equal(t, ptrAddress(&temperature), ptrAddress(call.Temperature))
	require.Equal(t, ptrAddress(&topP), ptrAddress(call.TopP))
	require.Equal(t, ptrAddress(&topK), ptrAddress(call.TopK))
	require.Equal(t, ptrAddress(&frequencyPenalty), ptrAddress(call.FrequencyPenalty))
	require.Equal(t, ptrAddress(&presencePenalty), ptrAddress(call.PresencePenalty))

	existingTemp := call.Temperature
	existingTopP := call.TopP
	existingTopK := call.TopK
	existingFreq := call.FrequencyPenalty
	existingPresence := call.PresencePenalty

	applyRuntimeConfig(&call, sessionAgentRuntimeConfig{})
	require.Equal(t, int64(1024), call.MaxOutputTokens)
	require.Equal(t, ptrAddress(existingTemp), ptrAddress(call.Temperature))
	require.Equal(t, ptrAddress(existingTopP), ptrAddress(call.TopP))
	require.Equal(t, ptrAddress(existingTopK), ptrAddress(call.TopK))
	require.Equal(t, ptrAddress(existingFreq), ptrAddress(call.FrequencyPenalty))
	require.Equal(t, ptrAddress(existingPresence), ptrAddress(call.PresencePenalty))
}

func TestUpdateSessionUsage_AccumulatesTotals(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{
		PromptTokens:     1000,
		CompletionTokens: 400,
		Cost:             1.25,
	}

	usage := fantasy.Usage{
		InputTokens:         120,
		CacheCreationTokens: 300,
		CacheReadTokens:     900,
		OutputTokens:        45,
	}

	agent.updateSessionUsage(model, &sess, usage, nil, 0, false)

	// PromptTokens tracks the current step's input tokens (current context
	// length), not a cumulative sum, to avoid double-counting history.
	require.Equal(t, int64(1320), sess.PromptTokens) // 120 + 300 + 900
	require.Equal(t, int64(445), sess.CompletionTokens)
	require.GreaterOrEqual(t, sess.Cost, 1.25)
	// LastPromptTokens should reflect only this step's input tokens (SET, not +=).
	require.Equal(t, int64(1320), sess.LastPromptTokens) // 120 + 300 + 900
}

func TestUpdateSessionUsage_AccumulatesTotals_AnthropicSDKProviderName(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{
		CatwalkCfg: catwalk.Model{},
		ModelCfg:   config.SelectedModel{Provider: "openai"},
		Model:      anthropicProviderLanguageModel{},
	}
	sess := session.Session{}

	usage := fantasy.Usage{
		InputTokens:         120,
		CacheCreationTokens: 300,
		CacheReadTokens:     900,
		OutputTokens:        45,
	}

	agent.updateSessionUsage(model, &sess, usage, nil, 0, false)

	// Model.Provider() = "@ai-sdk/anthropic" should be treated as Anthropic-style usage.
	require.Equal(t, int64(1320), sess.PromptTokens)
	require.Equal(t, int64(1320), sess.LastPromptTokens)
	require.Equal(t, int64(45), sess.CompletionTokens)
}

func TestUpdateSessionUsage_AccumulatesTotals_OpenAI(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "openai"}}
	sess := session.Session{
		PromptTokens:     1000,
		CompletionTokens: 400,
	}

	usage := fantasy.Usage{
		InputTokens:         120,
		CacheCreationTokens: 300,
		CacheReadTokens:     900,
		OutputTokens:        45,
	}

	agent.updateSessionUsage(model, &sess, usage, nil, 0, false)

	// PromptTokens tracks the current step's input tokens, not a cumulative sum.
	require.Equal(t, int64(1320), sess.PromptTokens)
	require.Equal(t, int64(445), sess.CompletionTokens)
	require.Equal(t, int64(1320), sess.LastPromptTokens)
}

func TestUpdateSessionUsage_LastPromptTokensIsSetNotAccumulated(t *testing.T) {
	t.Parallel()

	// Verify that LastPromptTokens always reflects the MOST RECENT step's
	// input tokens, not a cumulative sum. This is used for the context
	// window display and summarization threshold.
	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{}

	firstUsage := fantasy.Usage{
		InputTokens:  15000,
		OutputTokens: 200,
	}
	secondUsage := fantasy.Usage{
		InputTokens:  15300,
		OutputTokens: 180,
	}

	agent.updateSessionUsage(model, &sess, firstUsage, nil, 0, false)
	require.Equal(t, int64(15000), sess.LastPromptTokens)
	require.Equal(t, int64(15000), sess.PromptTokens)

	agent.updateSessionUsage(model, &sess, secondUsage, nil, 0, false)
	// PromptTokens reflects the current step's input tokens (current context
	// length), not a cumulative sum.
	require.Equal(t, int64(15300), sess.PromptTokens)
	// LastPromptTokens reflects only the second step (used for display/StopWhen).
	require.Equal(t, int64(15300), sess.LastPromptTokens)
}

func TestUpdateSessionUsage_FallbackToEstimatedTokens(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{}

	// Simulate a proxy that doesn't report input tokens in streaming mode.
	usage := fantasy.Usage{
		InputTokens:         0,
		CacheCreationTokens: 0,
		CacheReadTokens:     0,
		OutputTokens:        95,
	}

	estimatedTokens := int64(4200)
	agent.updateSessionUsage(model, &sess, usage, nil, estimatedTokens, false)

	// When API reports 0 input tokens, the estimated value should be used.
	require.Equal(t, estimatedTokens, sess.LastPromptTokens)
	// PromptTokens should also use the estimate.
	require.Equal(t, estimatedTokens, sess.PromptTokens)
	require.Equal(t, int64(95), sess.CompletionTokens)
}

func TestUpdateSessionUsage_PreferAPIOverEstimate(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{}

	// Real Anthropic correctly reports input tokens.
	usage := fantasy.Usage{
		InputTokens:         68,
		CacheCreationTokens: 4185,
		CacheReadTokens:     0,
		OutputTokens:        95,
	}

	estimatedTokens := int64(3500) // estimate is less accurate
	agent.updateSessionUsage(model, &sess, usage, nil, estimatedTokens, false)

	// API value (68+4185=4253) should be preferred over the estimate (3500)
	// because the API value exceeds the estimate.
	require.Equal(t, int64(4253), sess.LastPromptTokens)
	require.Equal(t, int64(4253), sess.PromptTokens)
}

func TestUpdateSessionUsage_FallbackWhenAPIUnderReports(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{}

	// Simulate a proxy that under-reports input tokens (e.g., only user
	// message tokens, omitting system prompt and tool definitions).
	usage := fantasy.Usage{
		InputTokens:  95,
		OutputTokens: 200,
	}

	estimatedTokens := int64(18500) // includes system prompt + tools
	agent.updateSessionUsage(model, &sess, usage, nil, estimatedTokens, false)

	// API reports 95 which is < estimatedTokens (18500), so the
	// estimate should be used instead.
	require.Equal(t, estimatedTokens, sess.LastPromptTokens)
	require.Equal(t, estimatedTokens, sess.PromptTokens)
}

func TestUpdateSessionUsage_FallbackWhenAPIReportsStaleValue(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{CatwalkCfg: catwalk.Model{}, ModelCfg: config.SelectedModel{Provider: "anthropic"}}
	sess := session.Session{}

	// Simulate a proxy that returns a constant (stale) input token count
	// that does not grow across tool-call steps.
	usage := fantasy.Usage{
		InputTokens:  5000,
		OutputTokens: 200,
	}

	estimatedTokens := int64(8000) // estimate grew with messages
	agent.updateSessionUsage(model, &sess, usage, nil, estimatedTokens, false)

	// API reports 5000 which is less than the estimate (8000). The
	// estimate should be used to keep the context display growing.
	require.Equal(t, estimatedTokens, sess.LastPromptTokens)
	require.Equal(t, estimatedTokens, sess.PromptTokens)
}

func TestEstimatePromptTokens(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		fantasy.NewSystemMessage(strings.Repeat("x", 3000)), // 3000 bytes
		fantasy.NewUserMessage("Hello world"),               // 11 bytes
	}

	// No agenttools. (3000 + 11) / 4 = 752.
	estimate := estimatePromptTokens(messages, nil)
	require.Equal(t, int64(752), estimate)

	// With a mock tool: name(9) + desc(31) + schema("null"=4) = 10 estimated
	// tokens, so total is 752 + 10 = 762.
	tool := &mockAgentTool{
		name:        "read_file",
		description: "Read a file from the filesystem",
	}
	estimateWithTools := estimatePromptTokens(messages, []fantasy.AgentTool{tool})
	require.Equal(t, int64(762), estimateWithTools)

	// ToolCallPart input is counted.
	msgs2 := []fantasy.Message{
		{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.ToolCallPart{Input: `{"path":"file.go"}`}}, // 19 bytes
		},
	}
	// 19 / 4 = 4.
	require.Equal(t, int64(4), estimatePromptTokens(msgs2, nil))

	// ToolResultPart text is counted.
	msgs3 := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("a", 400)}, // 400 bytes
			}},
		},
	}
	// 400 / 4 = 100.
	require.Equal(t, int64(100), estimatePromptTokens(msgs3, nil))

	// ToolResultPart media payloads use fixed image token estimate plus metadata text.
	msgsMedia := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				Output: fantasy.ToolResultOutputContentMedia{
					Data:      strings.Repeat("a", 64),
					MediaType: "image/png",
					Text:      "preview",
				},
			}},
		},
	}
	// image(2000) + media type(9)/4 + text(7)/4 => 2004 tokens.
	require.Equal(t, int64(2004), estimatePromptTokens(msgsMedia, nil))

	// FilePart attachments use fixed image token estimate plus metadata text.
	msgsFile := []fantasy.Message{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.FilePart{
				Filename:  "img.png",
				MediaType: "image/png",
				Data:      []byte("12345678"),
			}},
		},
	}
	// image(2000) + filename(7)/4 + media type(9)/4 => 2004 tokens.
	require.Equal(t, int64(2004), estimatePromptTokens(msgsFile, nil))

	msgs4 := []fantasy.Message{
		fantasy.NewUserMessage("你好世界"),
	}
	require.Equal(t, int64(4), estimatePromptTokens(msgs4, nil))
}

func TestEstimatePromptTokens_AggregatesShortASCIIFragments(t *testing.T) {
	t.Parallel()

	shortFragments := make([]fantasy.MessagePart, 8)
	for i := range shortFragments {
		shortFragments[i] = fantasy.TextPart{Text: "abc"}
	}

	messages := []fantasy.Message{
		{
			Role:    fantasy.MessageRoleUser,
			Content: shortFragments,
		},
	}

	require.Equal(t, int64(6), estimatePromptTokens(messages, nil))
}

func TestEstimatePromptTokens_ImageTokenEstimation(t *testing.T) {
	t.Parallel()

	// Non-image media types should not get the fixed image token estimate.
	msgsPDF := []fantasy.Message{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.FilePart{
				Filename:  "doc.pdf",
				MediaType: "application/pdf",
				Data:      []byte(strings.Repeat("x", 100)),
			}},
		},
	}
	// 100 bytes / 4 = 25 tokens for data, plus filename(7)/4 + media type(15)/4 = 30.
	require.Equal(t, int64(30), estimatePromptTokens(msgsPDF, nil))

	// Empty image data should not count as an image.
	msgsEmpty := []fantasy.Message{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.FilePart{
				Filename:  "empty.png",
				MediaType: "image/png",
				Data:      nil,
			}},
		},
	}
	// Only filename(9)/4 + media type(9)/4 = 4 tokens (integer division).
	require.Equal(t, int64(4), estimatePromptTokens(msgsEmpty, nil))

	// Multiple images should each get the fixed estimate.
	msgsMulti := []fantasy.Message{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.FilePart{
					Filename:  "img1.png",
					MediaType: "image/png",
					Data:      []byte(strings.Repeat("a", 10000)),
				},
				fantasy.FilePart{
					Filename:  "img2.jpg",
					MediaType: "image/jpeg",
					Data:      []byte(strings.Repeat("b", 20000)),
				},
			},
		},
	}
	// 2 images * 2000 = 4000, plus filename(7)/4 + media type(9)/4 * 2 = 8 => 4008.
	require.Equal(t, int64(4008), estimatePromptTokens(msgsMulti, nil))
}

// mockAgentTool implements fantasy.AgentTool for testing.
type mockAgentTool struct {
	name        string
	description string
	parallel    bool
}

func (m *mockAgentTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        m.name,
		Description: m.description,
		Parallel:    m.parallel,
	}
}

func (m *mockAgentTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}

func (m *mockAgentTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (m *mockAgentTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
func (m *mockAgentTool) SetParallel(parallel bool)                    { m.parallel = parallel }

type anthropicProviderLanguageModel struct {
	stubLanguageModel
}

func (anthropicProviderLanguageModel) Provider() string {
	return "@ai-sdk/anthropic"
}

func (anthropicProviderLanguageModel) Model() string {
	return "test-model"
}

func TestEnableNativeToolParallelism(t *testing.T) {
	t.Parallel()

	t.Run("enables parallel for read-only concurrency-safe tools", func(t *testing.T) {
		t.Parallel()
		tool := &mockAgentTool{name: "glob", description: "find files"}
		enableNativeToolParallelism(tool, agenttools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true})
		require.True(t, tool.Info().Parallel)
	})

	t.Run("keeps write tools sequential", func(t *testing.T) {
		t.Parallel()
		tool := &mockAgentTool{name: "edit", description: "modify files"}
		enableNativeToolParallelism(tool, agenttools.ToolMetadata{ReadOnly: false, ConcurrencySafe: true})
		require.False(t, tool.Info().Parallel)
	})
}

func TestTitleUserPromptFromCall(t *testing.T) {
	t.Parallel()

	t.Run("returns plain prompt", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "hello world", titleUserPromptFromCall("hello world"))
	})

	t.Run("extracts auto resume original prompt", func(t *testing.T) {
		t.Parallel()
		wrapped := autoResumePromptPrefix + "please fix bug`"
		require.Equal(t, "please fix bug", titleUserPromptFromCall(wrapped))
	})

	t.Run("extracts context window resume original prompt", func(t *testing.T) {
		t.Parallel()
		wrapped := contextWindowResumePromptPrefix + "analyze logs quickly`"
		require.Equal(t, "analyze logs quickly", titleUserPromptFromCall(wrapped))
	})

	t.Run("returns empty for blank prompt", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, titleUserPromptFromCall("   \n\t  "))
	})
}

func TestCleanGeneratedTitle(t *testing.T) {
	t.Parallel()

	t.Run("uses first non-empty line after think removal", func(t *testing.T) {
		t.Parallel()
		raw := "<think>hidden</think>\n\nUseful title\nSecond line"
		require.Equal(t, "Useful title", cleanGeneratedTitle(raw))
	})

	t.Run("removes quotes colons and normalizes whitespace", func(t *testing.T) {
		t.Parallel()
		raw := ` "Fix: parser bug"  `
		require.Equal(t, "Fix parser bug", cleanGeneratedTitle(raw))
	})

	t.Run("truncates to fifty runes", func(t *testing.T) {
		t.Parallel()
		raw := strings.Repeat("a", 60)
		require.Equal(t, strings.Repeat("a", 50), cleanGeneratedTitle(raw))
	})

	t.Run("falls back to default session name", func(t *testing.T) {
		t.Parallel()
		raw := `<think>only hidden</think>`
		require.Equal(t, DefaultSessionName, cleanGeneratedTitle(raw))
	})
}

func TestShouldGenerateSessionTitle(t *testing.T) {
	t.Parallel()

	require.True(t, shouldGenerateSessionTitle(""))
	require.True(t, shouldGenerateSessionTitle("New Session"))
	require.True(t, shouldGenerateSessionTitle("new session"))
	require.True(t, shouldGenerateSessionTitle(DefaultSessionName))
	require.False(t, shouldGenerateSessionTitle("Bugfix summary"))
}

func TestTitlePromptFromCallOrHistory(t *testing.T) {
	t.Parallel()

	t.Run("prefers current call prompt", func(t *testing.T) {
		t.Parallel()
		history := []message.Message{{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "older prompt"}}}}
		require.Equal(t, "latest prompt", titlePromptFromCallOrHistory("latest prompt", history))
	})

	t.Run("falls back to latest user history when call prompt empty", func(t *testing.T) {
		t.Parallel()
		history := []message.Message{
			{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "assistant"}}},
			{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first prompt"}}},
			{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "latest user prompt"}}},
		}
		require.Equal(t, "latest user prompt", titlePromptFromCallOrHistory("", history))
	})

	t.Run("returns empty when no usable prompt", func(t *testing.T) {
		t.Parallel()
		history := []message.Message{{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "assistant"}}}}
		require.Empty(t, titlePromptFromCallOrHistory("   ", history))
	})
}

func TestGenerateTitleResetsStreamedTitleOnModelFallback(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	streamCalls := 0
	titleModel := stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			streamCalls++
			if streamCalls == 1 {
				return func(yield func(fantasy.StreamPart) bool) {
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
						return
					}
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "partial-"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("small model failed")})
				}, nil
			}
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "clean-title"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}

	model := Model{
		Model: titleModel,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "claude-sonnet-4",
			Provider: "anthropic",
		},
	}

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	a.(*sessionAgent).generateTitle(t.Context(), testSession.ID, "user prompt", nil)

	after, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, "clean-title", after.Title)
	require.Equal(t, 2, streamCalls)
}

func TestGenerateTitleCleansAndTruncatesOutput(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	rawTitle := "\"Fix: parser bug in auth flow with very long suffix text\"\nextra line"
	titleModel := stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: rawTitle}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}

	model := Model{
		Model: titleModel,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "claude-sonnet-4",
			Provider: "anthropic",
		},
	}

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	a.(*sessionAgent).generateTitle(t.Context(), testSession.ID, "user prompt", nil)

	after, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, "Fix parser bug in auth flow with very long suffix", after.Title)
	require.LessOrEqual(t, utf8.RuneCountInString(after.Title), 50)
	require.NotContains(t, after.Title, "\"")
	require.NotContains(t, after.Title, ":")
}

func TestGenerateTitleDoesNotOverwriteSessionUsage(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	orig := testSession
	orig.PromptTokens = 321
	orig.CompletionTokens = 654
	orig.Cost = 12.34
	orig.LastPromptTokens = 11
	orig.LastCompletionTokens = 22
	_, err = env.sessions.Save(t.Context(), orig)
	require.NoError(t, err)

	titleModel := stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "kept-usage-title"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}

	model := Model{
		Model: titleModel,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "claude-sonnet-4",
			Provider: "anthropic",
		},
	}

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	a.(*sessionAgent).generateTitle(t.Context(), testSession.ID, "user prompt", nil)

	after, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, "kept-usage-title", after.Title)
	require.Equal(t, orig.PromptTokens, after.PromptTokens)
	require.Equal(t, orig.CompletionTokens, after.CompletionTokens)
	require.InDelta(t, orig.Cost, after.Cost, 1e-9)
	require.Equal(t, orig.LastPromptTokens, after.LastPromptTokens)
	require.Equal(t, orig.LastCompletionTokens, after.LastCompletionTokens)
}

func TestGenerateTitleRespectsSessionLockDuringUsageUpdate(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	titleModel := stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "locked-title"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}

	model := Model{
		Model: titleModel,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "claude-sonnet-4",
			Provider: "anthropic",
		},
	}

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	lock := &sync.Mutex{}
	lock.Lock()
	done := make(chan struct{})
	go func() {
		a.(*sessionAgent).generateTitle(t.Context(), testSession.ID, "user prompt", lock)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("generateTitle should wait for session lock")
	default:
	}

	lock.Unlock()
	<-done

	after, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, "locked-title", after.Title)
}

func TestRunDequeuesBeforeTitleGenerationCompletes(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	titleStarted := make(chan struct{})
	releaseTitle := make(chan struct{})
	var titleStartedOnce sync.Once
	titleModel := stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				titleStartedOnce.Do(func() { close(titleStarted) })
				<-releaseTitle
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "queued-safe-title"}) {
					return
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}

	model := Model{
		Model: titleModel,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "claude-sonnet-4",
			Provider: "anthropic",
		},
	}

	var sessAgent *sessionAgent
	testAgent := &queuePrepareTestAgent{t: t}
	sessAgent = NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "",
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return testAgent
		},
	}).(*sessionAgent)

	hasUserPrompt := func(prompt string) bool {
		msgs, listErr := env.messages.List(t.Context(), testSession.ID)
		require.NoError(t, listErr)
		for _, msg := range msgs {
			if msg.Role == message.User && msg.Content().Text == prompt {
				return true
			}
		}
		return false
	}

	testAgent.afterFirstPrepare = func() {
		_, runErr := sessAgent.Run(context.Background(), SessionAgentCall{
			SessionID:       testSession.ID,
			Prompt:          "queued later",
			MaxOutputTokens: 1000,
		})
		require.NoError(t, runErr)
	}

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sessAgent.Run(t.Context(), SessionAgentCall{
			SessionID:       testSession.ID,
			Prompt:          "run now",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	select {
	case <-titleStarted:
	case <-time.After(time.Second):
		t.Fatal("title generation did not start")
	}

	require.NoError(t, <-runDone)
	require.Eventually(t, func() bool {
		return hasUserPrompt("queued later")
	}, time.Second, 10*time.Millisecond)

	close(releaseTitle)
}

type textualToolProtocolTestAgent struct {
	t          *testing.T
	mu         sync.Mutex
	calls      int
	alwaysText bool
}

type deferredToolRuntimeStub struct {
	mu            sync.Mutex
	activated     map[string]map[string]struct{}
	knownDeferred map[string]bool
}

func (s *deferredToolRuntimeStub) activateDeferredToolsForSession(sessionID string, toolNames []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activated == nil {
		s.activated = make(map[string]map[string]struct{})
	}
	set := s.activated[sessionID]
	if set == nil {
		set = make(map[string]struct{})
		s.activated[sessionID] = set
	}
	activated := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		if _, ok := set[name]; !ok {
			set[name] = struct{}{}
		}
		activated = append(activated, name)
	}
	return activated
}

func (s *deferredToolRuntimeStub) activatedDeferredToolsForSession(sessionID string) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.activated[sessionID]
	if len(set) == 0 {
		return nil
	}
	clone := make(map[string]struct{}, len(set))
	for name := range set {
		clone[name] = struct{}{}
	}
	return clone
}

func (s *deferredToolRuntimeStub) isDeferredTool(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knownDeferred[name]
}

func (a *textualToolProtocolTestAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{}, nil
}

func (a *textualToolProtocolTestAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		_, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Messages: call.Messages})
		require.NoError(a.t, err)
	}

	a.mu.Lock()
	a.calls++
	attempt := a.calls
	a.mu.Unlock()

	if attempt == 1 || a.alwaysText {
		if call.OnTextDelta != nil {
			require.NoError(a.t, call.OnTextDelta(
				"assistant",
				"<|tool_calls_section_begin|><|tool_call_begin|>functions.read:15<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>",
			))
		}
		if call.OnStepFinish != nil {
			require.NoError(a.t, call.OnStepFinish(fantasy.StepResult{
				Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop},
			}))
		}
		return &fantasy.AgentResult{}, nil
	}

	if call.OnToolCall != nil {
		require.NoError(a.t, call.OnToolCall(fantasy.ToolCallContent{
			ToolCallID: "call-recovered",
			ToolName:   agenttools.ReadToolName,
			Input:      `{"file_path":"main.go"}`,
		}))
	}
	if call.OnToolResult != nil {
		require.NoError(a.t, call.OnToolResult(fantasy.ToolResultContent{
			ToolCallID: "call-recovered",
			ToolName:   agenttools.ReadToolName,
			Result: fantasy.ToolResultOutputContentText{
				Text: "module example.com/recovered",
			},
		}))
	}
	if call.OnStepFinish != nil {
		require.NoError(a.t, call.OnStepFinish(fantasy.StepResult{
			Response: fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls},
		}))
	}
	return &fantasy.AgentResult{}, nil
}

func (a *textualToolProtocolTestAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func TestShouldRetryForTextualToolCallProtocolAllowsToolUseWithoutStructuredToolCalls(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "<|tool_calls_section_begin|><|tool_call_begin|>functions.read<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>"},
			message.Finish{Reason: message.FinishReasonToolUse},
		},
	}

	require.True(t, shouldRetryForTextualToolCallProtocol(msg))
}

func TestShouldRetryForTextualToolCallProtocolDetectsReasoning(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "<|tool_calls_section_begin|><|tool_call_begin|>functions.read:25<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}

	require.True(t, shouldRetryForTextualToolCallProtocol(msg))
}

func TestParseTextualToolCallsFromAssistantUsesAnthropicSafeIDs(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "<|tool_calls_section_begin|><|tool_call_begin|>functions.read:25<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>"},
		},
	}

	toolCalls := parseTextualToolCallsFromAssistant(msg)

	require.Len(t, toolCalls, 1)
	require.Equal(t, "functions_read_25", toolCalls[0].ID)
	require.Equal(t, agenttools.ReadToolName, toolCalls[0].Name)
	require.Equal(t, `{"file_path":"main.go"}`, toolCalls[0].Input)
}

func TestStripTextualToolCallProtocolFromAssistant(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "I will inspect it.\n<|tool_calls_section_begin|><|tool_call_begin|>functions.read:6<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>"},
			message.ReasoningContent{Thinking: "Thinking first.\n<|tool_calls_section_begin|><|tool_call_begin|>functions.grep:7<|tool_call_argument_begin|>{\"pattern\":\"x\"}<|tool_call_end|><|tool_calls_section_end|>"},
			message.ToolCall{ID: "call-1", Name: agenttools.ReadToolName, Input: `{"file_path":"main.go"}`, Finished: true},
		},
	}

	require.True(t, stripTextualToolCallProtocolFromAssistant(msg))
	require.Equal(t, "I will inspect it.", msg.Content().Text)
	require.Equal(t, "Thinking first.", msg.ReasoningContent().Thinking)
}

func TestSanitizeAnthropicToolCallIDsInMessages(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{
					ToolCallID: "functions.grep:64",
					ToolName:   agenttools.GrepToolName,
					Input:      `{"pattern":"x"}`,
				},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "functions.grep:64",
					Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
				},
				fantasy.ToolResultPart{
					ToolCallID: "safe_id-1",
					Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
				},
			},
		},
	}

	sanitized, changed, count := sanitizeAnthropicToolCallIDsInMessages(messages)

	require.True(t, changed)
	require.Equal(t, 2, count)
	require.Equal(t, "functions.grep:64", messages[0].Content[0].(fantasy.ToolCallPart).ToolCallID)
	require.Equal(t, "functions_grep_64", sanitized[0].Content[0].(fantasy.ToolCallPart).ToolCallID)
	require.Equal(t, "functions_grep_64", sanitized[1].Content[0].(fantasy.ToolResultPart).ToolCallID)
	require.Equal(t, "safe_id-1", sanitized[1].Content[1].(fantasy.ToolResultPart).ToolCallID)
}

func TestConvertToToolResult_PreservesGenericRecoveryMetadata(t *testing.T) {
	t.Parallel()

	metadata := `{"recovered_by":"literal_text_fallback","recovery_action":"Pattern was not valid regex syntax. Treated it as literal text instead.","fallback_tool":"grep","fallback_tool_query":"[]fantasy.AgentTool","recovered_parameters":["literal_text"]}`
	result := (&sessionAgent{}).convertToToolResult(context.Background(), fantasy.ToolResultContent{
		ToolCallID:     "call-1",
		ToolName:       agenttools.GrepToolName,
		ClientMetadata: metadata,
		Result:         fantasy.ToolResultOutputContentText{Text: "Found 1 matches"},
	})

	require.False(t, result.IsError)
	state, ok := result.DeferredToolState()
	require.True(t, ok)
	require.Equal(t, "Pattern was not valid regex syntax. Treated it as literal text instead.", state.RecoveryAction)
	require.Equal(t, "grep", state.FallbackTool)
	require.Equal(t, "[]fantasy.AgentTool", state.FallbackToolQuery)
	require.Equal(t, []string{"literal_text"}, state.RecoveredParameters)
}

func TestConvertToToolResult_PreservesDeferredToolErrorRecoveryMetadata(t *testing.T) {
	t.Parallel()

	payload := `{"recovered_by":"deferred_tool_not_activated","tool":"sourcegraph","recovery_action":"Run tool_search with query \"select:sourcegraph\" before using this tool.","fallback_tool":"tool_search","fallback_tool_query":"select:sourcegraph","recovered_parameters":["query"]}`
	result := (&sessionAgent{}).convertToToolResult(context.Background(), fantasy.ToolResultContent{
		ToolCallID:     "call-1",
		ToolName:       "sourcegraph",
		ClientMetadata: payload,
		Result:         fantasy.ToolResultOutputContentError{Error: errors.New("not activated")},
	})

	require.True(t, result.IsError)
	state, ok := result.DeferredToolState()
	require.True(t, ok)
	require.Equal(t, "sourcegraph", state.RecoveredTool)
	require.Equal(t, "tool_search", state.FallbackTool)
	require.Equal(t, "select:sourcegraph", state.FallbackToolQuery)
	require.Equal(t, []string{"query"}, state.RecoveredParameters)
}

func TestRunRecoversFromTextualToolCallProtocol(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "Text protocol recover")
	require.NoError(t, err)

	model := Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Model: "test", Provider: "test"},
	}

	testAgent := &textualToolProtocolTestAgent{t: t}
	sessAgent := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "",
		WorkingDir:           env.workingDir,
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return testAgent
		},
	}).(*sessionAgent)

	_, err = sessAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       testSession.ID,
		Prompt:          "please inspect and fix",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, 2, testAgent.callCount())

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)

	foundRecoveredToolResult := false
	for _, msg := range msgs {
		if msg.Role == message.Assistant {
			require.NotContains(t, msg.Content().Text, "<|tool_calls_section_begin|>")
		}
		if msg.Role != message.Tool {
			continue
		}
		for _, tr := range msg.ToolResults() {
			if tr.ToolCallID == "call-recovered" {
				foundRecoveredToolResult = true
				require.False(t, tr.IsError)
				require.Contains(t, tr.Content, "module example.com/recovered")
			}
		}
	}
	require.True(t, foundRecoveredToolResult)
}

func TestRunFailsAfterRepeatedTextualToolCallProtocol(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "Text protocol fail")
	require.NoError(t, err)

	model := Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Model: "test", Provider: "test"},
	}

	testAgent := &textualToolProtocolTestAgent{t: t, alwaysText: true}
	sessAgent := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "",
		WorkingDir:           env.workingDir,
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return testAgent
		},
	}).(*sessionAgent)

	_, err = sessAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       testSession.ID,
		Prompt:          "please inspect and fix",
		MaxOutputTokens: 1000,
	})
	require.ErrorContains(t, err, "model repeatedly emitted the same textual tool-call protocol instead of structured tool calls")
	require.Equal(t, maxRepeatedTextualToolProtocolRecoveries+1, testAgent.callCount())

	msgs, err := env.messages.List(t.Context(), testSession.ID)
	require.NoError(t, err)

	assistantCount := 0
	for _, msg := range msgs {
		if msg.Role == message.Assistant {
			assistantCount++
			require.NotContains(t, msg.Content().Text, "<|tool_calls_section_begin|>")
			require.NotContains(t, msg.ReasoningContent().Thinking, "<|tool_calls_section_begin|>")
		}
		if msg.Role == message.Tool {
			for _, tr := range msg.ToolResults() {
				require.NotEqual(t, "call-recovered", tr.ToolCallID)
			}
		}
	}
	require.Equal(t, maxRepeatedTextualToolProtocolRecoveries, assistantCount)
}

func TestPreparePromptRestoresDeferredToolActivationsFromHistory(t *testing.T) {
	t.Parallel()

	runtime := &deferredToolRuntimeStub{}
	a := &sessionAgent{deferredToolRuntime: runtime}
	_, _ = a.preparePrompt([]message.Message{
		{
			SessionID:              "session-1",
			Role:                   message.Assistant,
			ActivatedDeferredTools: []string{"sourcegraph"},
			Parts: []message.ContentPart{
				message.TextContent{Text: "done"},
			},
		},
		{
			SessionID: "session-1",
			Role:      message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: agenttools.ToolSearchToolName, Content: "{}"}.WithDeferredToolState(message.ToolResultDeferredToolState{ActivatedTools: []string{"mcp_acemcp_search_context"}}),
			},
		},
	})

	activated := a.currentActivatedDeferredTools("session-1")
	require.Equal(t, []string{"mcp_acemcp_search_context", "sourcegraph"}, activated)
}

func TestPreparePromptDropsOrphanedToolResultsWithoutMatchingToolCall(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call-1", Name: agenttools.ReadToolName, Input: `{"file_path":"main.go"}`, Finished: true},
			},
		},
		{
			ID:   "tool-valid",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: agenttools.ReadToolName, Content: "ok"},
			},
		},
		{
			ID:   "tool-orphan",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-orphan", Name: agenttools.ReadToolName, Content: "orphan"},
			},
		},
	})

	var toolResultIDs []string
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			toolResultPart, ok := part.(fantasy.ToolResultPart)
			if !ok {
				continue
			}
			toolResultIDs = append(toolResultIDs, toolResultPart.ToolCallID)
		}
	}

	require.Contains(t, toolResultIDs, "call-1")
	require.NotContains(t, toolResultIDs, "call-orphan")
}

func TestPreparePromptInjectsSyntheticToolResultForMissingToolCallResult(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call-missing", Name: agenttools.ReadToolName, Input: `{"file_path":"main.go"}`, Finished: true},
			},
		},
	})

	foundSynthetic := false
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			toolResultPart, ok := part.(fantasy.ToolResultPart)
			if !ok || toolResultPart.ToolCallID != "call-missing" {
				continue
			}
			errOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](toolResultPart.Output)
			require.True(t, ok)
			require.ErrorContains(t, errOutput.Error, "tool execution was interrupted")
			foundSynthetic = true
		}
	}
	require.True(t, foundSynthetic)
}

func TestPreparePromptDropsLateToolResultForEarlierAssistantCall(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call-1", Name: agenttools.GlobToolName, Input: `{"path":"**/*.test.ts"}`, Finished: true},
			},
		},
		{
			ID:   "assistant-error",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Invalid parameters"},
			},
		},
		{
			ID:   "tool-late",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: agenttools.GlobToolName, Content: "No files found"},
			},
		},
	})

	var toolResultIDs []string
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			toolResultPart, ok := part.(fantasy.ToolResultPart)
			if !ok {
				continue
			}
			toolResultIDs = append(toolResultIDs, toolResultPart.ToolCallID)
		}
	}

	require.Len(t, toolResultIDs, 1)
	require.Equal(t, "call-1", toolResultIDs[0])
	errOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](history[1].Content[0].(fantasy.ToolResultPart).Output)
	require.True(t, ok)
	require.ErrorContains(t, errOutput.Error, "tool execution was interrupted")
}

func TestPreparePromptDropsCanceledAssistantToolBranchBeforeNextUser(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "investigate bug"},
			},
		},
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "checking files"},
				message.ToolCall{ID: "call-1", Name: agenttools.ReadToolName, Input: `{"file_path":"main.go"}`, Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
		{
			ID:   "tool-1",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: agenttools.ReadToolName, Content: "ok"},
			},
		},
		{
			ID:   "assistant-canceled",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.Finish{Reason: message.FinishReasonCanceled},
			},
		},
		{
			ID:   "user-2",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "actually check repo B"},
			},
		},
	})

	var userTexts []string
	var systemTexts []string
	var toolCallIDs []string
	var toolResultIDs []string
	for _, msg := range history {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			for _, part := range msg.Content {
				textPart, ok := part.(fantasy.TextPart)
				if !ok {
					continue
				}
				userTexts = append(userTexts, textPart.Text)
			}
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				textPart, ok := part.(fantasy.TextPart)
				if !ok {
					continue
				}
				systemTexts = append(systemTexts, textPart.Text)
			}
		case fantasy.MessageRoleAssistant:
			for _, part := range msg.Content {
				toolCallPart, ok := part.(fantasy.ToolCallPart)
				if !ok {
					continue
				}
				toolCallIDs = append(toolCallIDs, toolCallPart.ToolCallID)
			}
		case fantasy.MessageRoleTool:
			for _, part := range msg.Content {
				toolResultPart, ok := part.(fantasy.ToolResultPart)
				if !ok {
					continue
				}
				toolResultIDs = append(toolResultIDs, toolResultPart.ToolCallID)
			}
		}
	}

	require.Equal(t, []string{"investigate bug", "actually check repo B"}, userTexts)
	require.Contains(t, systemTexts, canceledPromptBranchSystemNote)
	require.NotContains(t, toolCallIDs, "call-1")
	require.NotContains(t, toolResultIDs, "call-1")
}

func TestPreparePromptKeepsLatestCanceledBranchWithoutLaterUser(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "investigate bug"},
			},
		},
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "checking files"},
				message.ToolCall{ID: "call-1", Name: agenttools.ReadToolName, Input: `{"file_path":"main.go"}`, Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
		{
			ID:   "tool-1",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: agenttools.ReadToolName, Content: "ok"},
			},
		},
		{
			ID:   "assistant-canceled",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.Finish{Reason: message.FinishReasonCanceled},
			},
		},
	})

	var toolCallIDs []string
	var toolResultIDs []string
	var systemTexts []string
	for _, msg := range history {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				textPart, ok := part.(fantasy.TextPart)
				if !ok {
					continue
				}
				systemTexts = append(systemTexts, textPart.Text)
			}
		case fantasy.MessageRoleAssistant:
			for _, part := range msg.Content {
				toolCallPart, ok := part.(fantasy.ToolCallPart)
				if !ok {
					continue
				}
				toolCallIDs = append(toolCallIDs, toolCallPart.ToolCallID)
			}
		case fantasy.MessageRoleTool:
			for _, part := range msg.Content {
				toolResultPart, ok := part.(fantasy.ToolResultPart)
				if !ok {
					continue
				}
				toolResultIDs = append(toolResultIDs, toolResultPart.ToolCallID)
			}
		}
	}

	require.Contains(t, toolCallIDs, "call-1")
	require.Contains(t, toolResultIDs, "call-1")
	require.NotContains(t, systemTexts, canceledPromptBranchSystemNote)
}

// TestPreparePromptDropsOrphanedUnfinishedAssistantBeforeNextUser verifies
// that an assistant turn left without any Finish part (e.g. ESC interrupted
// the agent before cleanup could persist a finish reason) is dropped when a
// later user message exists. Otherwise its partially-streamed content —
// often a thinking block without a signature — would be sent back to strict
// thinking-mode proxies and rejected with errors like "content[].thinking
// in the thinking mode must be passed back to the API".
func TestPreparePromptDropsOrphanedUnfinishedAssistantBeforeNextUser(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first ask"},
			},
		},
		{
			ID:   "assistant-orphan",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "partial unsigned thoughts"},
				message.TextContent{Text: "partial answer"},
			},
		},
		{
			ID:   "user-2",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "second ask"},
			},
		},
	})

	var userTexts []string
	var assistantTexts []string
	var systemTexts []string
	for _, msg := range history {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			for _, part := range msg.Content {
				if textPart, ok := part.(fantasy.TextPart); ok {
					userTexts = append(userTexts, textPart.Text)
				}
			}
		case fantasy.MessageRoleAssistant:
			for _, part := range msg.Content {
				if textPart, ok := part.(fantasy.TextPart); ok {
					assistantTexts = append(assistantTexts, textPart.Text)
				}
			}
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				if textPart, ok := part.(fantasy.TextPart); ok {
					systemTexts = append(systemTexts, textPart.Text)
				}
			}
		}
	}

	require.Equal(t, []string{"first ask", "second ask"}, userTexts)
	require.NotContains(t, assistantTexts, "partial answer")
	require.Contains(t, systemTexts, canceledPromptBranchSystemNote)
}

// TestPreparePromptKeepsUnfinishedAssistantWhenItIsTheLastTurn verifies that
// the orphan-cleanup heuristic only fires when a later user message exists.
// Without a later user turn there is nothing to "branch away from", so the
// assistant message is preserved.
func TestPreparePromptKeepsUnfinishedAssistantWhenItIsTheLastTurn(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first ask"},
			},
		},
		{
			ID:   "assistant-orphan",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "in-progress reply"},
			},
		},
	})

	var assistantTexts []string
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			if textPart, ok := part.(fantasy.TextPart); ok {
				assistantTexts = append(assistantTexts, textPart.Text)
			}
		}
	}

	require.Contains(t, assistantTexts, "in-progress reply")
}

// TestPreparePromptKeepsRoundTrippedAssistantBeforeNextUser verifies that an
// assistant turn that has been round-tripped through fantasy.Message
// conversion (which loses the persisted Finish part) is NOT mistaken for an
// orphaned/interrupted turn and dropped. PrepareStep performs this round
// trip when calling builtinPruneToolResults or messages.transform plugins,
// so this regression test ensures prior turns survive when a new user
// message arrives.
func TestPreparePromptKeepsRoundTrippedAssistantBeforeNextUser(t *testing.T) {
	t.Parallel()

	// Build a completed prior turn via the same path PrepareStep uses:
	// internal -> fantasy -> internal. The conversion drops the Finish part,
	// which previously caused trimCanceledPromptBranches to nuke the entire
	// prior turn.
	prior := []message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first ask"},
			},
		},
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first reply"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}
	var fantasyMsgs []fantasy.Message
	for _, m := range prior {
		fantasyMsgs = append(fantasyMsgs, m.ToAIMessage()...)
	}
	roundTripped := message.FromFantasyMessages(fantasyMsgs)
	roundTripped = append(roundTripped, message.Message{
		ID:   "user-2",
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "second ask"},
		},
	})

	a := &sessionAgent{}
	history, _ := a.preparePrompt(roundTripped)

	var userTexts, assistantTexts, systemTexts []string
	for _, msg := range history {
		for _, part := range msg.Content {
			tp, ok := part.(fantasy.TextPart)
			if !ok {
				continue
			}
			switch msg.Role {
			case fantasy.MessageRoleUser:
				userTexts = append(userTexts, tp.Text)
			case fantasy.MessageRoleAssistant:
				assistantTexts = append(assistantTexts, tp.Text)
			case fantasy.MessageRoleSystem:
				systemTexts = append(systemTexts, tp.Text)
			}
		}
	}

	require.Equal(t, []string{"first ask", "second ask"}, userTexts)
	require.Contains(t, assistantTexts, "first reply",
		"prior assistant reply was incorrectly dropped after round-tripping through fantasy.Message")
	require.NotContains(t, systemTexts, canceledPromptBranchSystemNote,
		"canceled-prompt boundary note was injected for a completed prior turn")
}

func TestConvertToToolResult_OrchestrateModeSemanticInterceptor(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	// Create an orchestrate mode session
	sess, err := env.sessions.Create(t.Context(), "Test Session")
	require.NoError(t, err)
	sess.CollaborationMode = session.CollaborationModeOrchestrate
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
			ModelCfg:   config.SelectedModel{Model: "test", Provider: "test"},
		},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	ctx := context.WithValue(context.Background(), agenttools.SessionIDContextKey, sess.ID)

	// Simulate "tool not found: edit" error
	result := a.convertToToolResult(ctx, fantasy.ToolResultContent{
		ToolCallID: "call-1",
		ToolName:   "edit",
		Result:     fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: edit")},
	})

	require.True(t, result.IsError)
	require.Contains(t, result.Content, "Failed: The \"edit\" tool is not available in Orchestrate (coordinator) mode.")
	require.Contains(t, result.Content, "Every file mutation and code implementation MUST go through a specialized subagent")
}

func TestBuildChatRequestState_OrchestrateModeAttentionDefense(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "Test Session")
	require.NoError(t, err)
	sess.CollaborationMode = session.CollaborationModeOrchestrate
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	a := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
			ModelCfg:   config.SelectedModel{Model: "test", Provider: "test"},
		},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	// Build inputs with User messages
	userMsg := message.Message{
		SessionID: sess.ID,
		Role:      message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "please edit code"},
		},
	}

	state, err := a.buildChatRequestState(t.Context(), chatRequestStateInput{
		SessionID:    sess.ID,
		Agent:        "session",
		Model:        a.largeModel.Get(),
		Messages:     []message.Message{userMsg},
		SystemPrompt: "system prompt",
	})
	require.NoError(t, err)

	// Verify that the caveat has been prepended to user message in memory
	require.Len(t, state.Messages, 1)
	require.Contains(t, state.Messages[0].Content().Text, "<system_intent_gate_caveat>")
	require.Contains(t, state.Messages[0].Content().Text, "please edit code")

	// Verify idempotency: calling it again with already-prefixed message should not double prefix
	state2, err := a.buildChatRequestState(t.Context(), chatRequestStateInput{
		SessionID:    sess.ID,
		Agent:        "session",
		Model:        a.largeModel.Get(),
		Messages:     state.Messages,
		SystemPrompt: "system prompt",
	})
	require.NoError(t, err)
	require.Len(t, state2.Messages, 1)
	text := state2.Messages[0].Content().Text
	require.Equal(t, 1, strings.Count(text, "<system_intent_gate_caveat>"))
}

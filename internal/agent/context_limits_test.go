package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestContextWindowLimitsMinOfWindowAndMaxPromptTokens(t *testing.T) {
	t.Parallel()
	limits := ContextWindowLimitsFor(catwalk.Model{
		ContextWindow: 400_000,
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
		},
	})
	require.Equal(t, int64(400_000), limits.ContextWindow)
	require.Equal(t, int64(150_000), limits.MaxPromptTokens)
	require.Equal(t, int64(150_000), limits.EffectiveContextWindow)
}

func TestContextWindowLimitsMaxPromptTokensAloneWhenWindowZero(t *testing.T) {
	t.Parallel()
	limits := ContextWindowLimitsFor(catwalk.Model{
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
		},
	})
	require.Equal(t, int64(0), limits.ContextWindow)
	require.Equal(t, int64(150_000), limits.MaxPromptTokens)
	require.Equal(t, int64(150_000), limits.EffectiveContextWindow)
}

func TestContextWindowLimitsRawWindowWhenNoOption(t *testing.T) {
	t.Parallel()
	limits := ContextWindowLimitsFor(catwalk.Model{ContextWindow: 200_000})
	require.Equal(t, int64(200_000), limits.ContextWindow)
	require.Equal(t, int64(0), limits.MaxPromptTokens)
	require.Equal(t, int64(200_000), limits.EffectiveContextWindow)
}

func TestContextWindowLimitsWindowSmallerThanMaxPrompt(t *testing.T) {
	t.Parallel()
	limits := ContextWindowLimitsFor(catwalk.Model{
		ContextWindow: 100_000,
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 200_000},
		},
	})
	require.Equal(t, int64(100_000), limits.EffectiveContextWindow)
}

func TestEffectiveContextWindowWrapper(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(150_000), EffectiveContextWindow(catwalk.Model{
		ContextWindow: 400_000,
		Options: catwalk.ModelOptions{
			ProviderOptions: map[string]any{"max_prompt_tokens": 150_000},
		},
	}))
}

func TestSummaryHistoryTokenBudgetUsesMaxPromptTokens(t *testing.T) {
	t.Parallel()

	model := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    400_000,
			DefaultMaxTokens: 32_000,
			Options: catwalk.ModelOptions{
				ProviderOptions: map[string]any{"max_prompt_tokens": 100_000},
			},
		},
	}

	overhead := estimateStringTokens("summary prompt") +
		estimateStringTokens("summary system") +
		estimateStringTokens("prefix")
	require.Equal(t, int64(80_000)-overhead, summaryHistoryTokenBudget(model, 0, "summary prompt", "summary system", "prefix"))
}

func TestSummaryHistoryTokenBudgetSubtractsExplicitOutputReserve(t *testing.T) {
	t.Parallel()

	model := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    100_000,
			DefaultMaxTokens: 50_000,
		},
	}

	require.Equal(t, int64(95_000), summaryHistoryTokenBudget(model, 5_000, "", "", ""))
}

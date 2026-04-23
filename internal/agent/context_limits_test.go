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

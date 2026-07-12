package httpext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrimChainedInput(t *testing.T) {
	t.Parallel()

	full := []any{map[string]any{"role": "user"}, map[string]any{"role": "tool"}}
	require.Equal(t, full, trimChainedInput(full, 0))
	require.Equal(t, []any{map[string]any{"role": "tool"}}, trimChainedInput(full, 1))
	require.Equal(t, full, trimChainedInput(full, 99))
	require.Equal(t, "x", trimChainedInput("x", 1))
}
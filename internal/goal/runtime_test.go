package goal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsSteerPrompt(t *testing.T) {
	t.Parallel()

	require.True(t, IsSteerPrompt("Continue work on the active goal.\n\nObjective: ship it"))
	require.True(t, IsSteerPrompt("Token budget exhausted for the active goal.\n\nObjective: ship it"))
	require.False(t, IsSteerPrompt("Fix the failing test in coordinator.go"))
}

func TestShouldChainContinuation(t *testing.T) {
	t.Parallel()

	require.True(t, ShouldChainContinuation("Continue work on the active goal.", 3))
	require.True(t, ShouldChainContinuation("<guided_goal>\nRefine this goal\n</guided_goal>", 0))
	require.False(t, ShouldChainContinuation("Please add logging to the handler", 0))
	require.False(t, ShouldChainContinuation("Please add logging to the handler", 2))
}

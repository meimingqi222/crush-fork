package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorMemoryFreshness(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := &coordinator{longTermMemory: env.memory}

	status, err := coord.MemoryFreshness(context.Background())
	require.NoError(t, err)
	require.False(t, status.HasMemories)
	require.Empty(t, status.Warning)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: "project/context", Value: "Durable project context"}))

	status, err = coord.MemoryFreshness(context.Background())
	require.NoError(t, err)
	require.True(t, status.HasMemories)
}

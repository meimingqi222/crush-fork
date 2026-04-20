package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

func TestApplyExtractedMemoriesSupportsDelete(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(context.Background(), memory.StoreParams{
		Key:   "project/obsolete",
		Value: "# Old\n\nobsolete",
		Type:  "project",
	}))

	err = applyExtractedMemories(context.Background(), memorySvc, []extractedMemory{
		{Action: "delete", Key: "project/obsolete"},
	})
	require.NoError(t, err)

	_, err = memorySvc.Get(context.Background(), "project/obsolete")
	require.ErrorIs(t, err, memory.ErrNotFound)
}

func TestApplyExtractedMemoriesStoresContent(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)

	err = applyExtractedMemories(context.Background(), memorySvc, []extractedMemory{
		{
			Action:      "update",
			Key:         "user/style",
			Description: "Concise responses",
			Content:     "User prefers concise responses.",
			Type:        "user",
			Scope:       "project",
		},
	})
	require.NoError(t, err)

	entry, err := memorySvc.Get(context.Background(), "user/style")
	require.NoError(t, err)
	require.Equal(t, "user", entry.Type)
	require.Equal(t, "project", entry.Scope)
	require.Contains(t, entry.Value, "# Concise responses")
	require.Contains(t, entry.Value, "User prefers concise responses.")
}

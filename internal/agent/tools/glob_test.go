package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGlobFilesMatchesHiddenBasenameFiles(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	hiddenJSON := filepath.Join(tempDir, ".usersetting.json")
	visibleJSON := filepath.Join(tempDir, "settings.json")
	nestedHiddenJSON := filepath.Join(tempDir, "config", ".usersetting.json")
	repoDir := filepath.Join(tempDir, ".git")
	ignoredDir := filepath.Join(tempDir, "node_modules")

	for _, file := range []string{hiddenJSON, visibleJSON, nestedHiddenJSON} {
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte("{}"), 0o644))
	}
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "config.json"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(ignoredDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ignoredDir, ".package.json"), []byte("{}"), 0o644))

	matches, truncated, err := globFiles(t.Context(), "*.json", tempDir, 0)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{hiddenJSON, visibleJSON}, matches)

	matches, truncated, err = globFiles(t.Context(), "**/*.json", tempDir, 0)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{hiddenJSON, visibleJSON, nestedHiddenJSON}, matches)
}

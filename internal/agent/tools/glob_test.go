package tools

import (
	"fmt"
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

	matches, truncated, err := globFiles(t.Context(), "*.json", tempDir, 0, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{hiddenJSON, visibleJSON}, matches)

	matches, truncated, err = globFiles(t.Context(), "**/*.json", tempDir, 0, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{hiddenJSON, visibleJSON, nestedHiddenJSON}, matches)
}

func TestGlobFilesGitignoreFalse(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	ignoredFile := filepath.Join(tempDir, ".env.local")
	gitignore := filepath.Join(tempDir, ".gitignore")

	require.NoError(t, os.WriteFile(ignoredFile, []byte("secret"), 0o644))
	require.NoError(t, os.WriteFile(gitignore, []byte(".env*\n"), 0o644))

	matches, truncated, err := globFiles(t.Context(), ".env*", tempDir, 0, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, matches)

	matches, truncated, err = globFiles(t.Context(), ".env*", tempDir, 0, true, false)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{ignoredFile}, matches)
}

func TestGlobFilesHiddenFalse(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	hiddenFile := filepath.Join(tempDir, ".hidden.json")
	visibleFile := filepath.Join(tempDir, "visible.json")

	require.NoError(t, os.WriteFile(hiddenFile, []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(visibleFile, []byte("{}"), 0o644))

	matches, truncated, err := globFiles(t.Context(), "*.json", tempDir, 0, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{hiddenFile, visibleFile}, matches)

	matches, truncated, err = globFiles(t.Context(), "*.json", tempDir, 0, false, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{visibleFile}, matches)
}

func TestGlobFilesLimit(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	for i := range 5 {
		name := fmt.Sprintf("file%d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(tempDir, name), []byte("x"), 0o644))
	}

	matches, truncated, err := globFiles(t.Context(), "*.txt", tempDir, 2, true, true)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, matches, 2)

	matches, truncated, err = globFiles(t.Context(), "*.txt", tempDir, 0, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, matches, 5)
}

func TestGlobForPathAbsolutePath(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file.txt"), []byte("x"), 0o644))

	files, truncated, err := globForPath(t.Context(), tempDir, tempDir, 10, true, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, files, 1)
	require.Equal(t, "file.txt", files[0])
}

func TestParseFindPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		path            string
		wantBase        string
		wantGlobPattern string
		wantHasGlob     bool
	}{
		{
			name:            "simple pattern becomes recursive",
			path:            "*.ts",
			wantBase:        ".",
			wantGlobPattern: "**/*.ts",
			wantHasGlob:     true,
		},
		{
			name:            "already recursive unchanged",
			path:            "**/*.rs",
			wantBase:        ".",
			wantGlobPattern: "**/*.rs",
			wantHasGlob:     true,
		},
		{
			name:            "path with slash",
			path:            "src/*.ts",
			wantBase:        "src",
			wantGlobPattern: "*.ts",
			wantHasGlob:     true,
		},
		{
			name:            "directory without glob",
			path:            "src",
			wantBase:        "src",
			wantGlobPattern: "**/*",
			wantHasGlob:     false,
		},
		{
			name:            "backslashes normalized",
			path:            "src\\**\\*.ts",
			wantBase:        "src",
			wantGlobPattern: "**/*.ts",
			wantHasGlob:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			basePath, globPattern, hasGlob := parseFindPattern(tt.path)
			require.Equal(t, tt.wantBase, basePath)
			require.Equal(t, tt.wantGlobPattern, globPattern)
			require.Equal(t, tt.wantHasGlob, hasGlob)
		})
	}
}

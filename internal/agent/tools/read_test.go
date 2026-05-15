package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/stretchr/testify/require"
)

type readToolFileTracker struct {
	reads []string
}

func (f *readToolFileTracker) RecordRead(_ context.Context, _ string, path string) {
	f.reads = append(f.reads, path)
}

func (f *readToolFileTracker) LastReadTime(context.Context, string, string) time.Time {
	return time.Time{}
}

func (f *readToolFileTracker) ListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ filetracker.Service = (*readToolFileTracker)(nil)

func runReadToolForTest(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ReadParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "read-call",
		Name:  ReadToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

func newReadToolForTest(workingDir string, tracker filetracker.Service) fantasy.AgentTool {
	return NewReadTool(nil, nil, tracker, workingDir, config.ToolLs{}, nil)
}

func parseReadMetadata(t *testing.T, resp fantasy.ToolResponse) ReadResponseMetadata {
	t.Helper()

	var meta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func TestReadToolRecoversUniqueSuffixMatch(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	actualPath := filepath.Join(workingDir, "internal", "ui", "dialog", "render.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(actualPath), 0o755))
	require.NoError(t, os.WriteFile(actualPath, []byte("package dialog\n"), 0o644))

	tracker := &readToolFileTracker{}
	tool := newReadToolForTest(workingDir, tracker)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: "dialog/render.go"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "package dialog")

	meta := parseReadMetadata(t, resp)
	require.Equal(t, "unique_suffix_recovery", meta.RecoveredBy)
	require.Contains(t, meta.RecoveryAction, "Recovered missing read path")
	require.Equal(t, []string{actualPath}, tracker.reads)
}

func TestReadToolMissingPathReturnsGroundingAdviceAndDoesNotRecordRead(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, "internal", "ui", "dialog"), 0o755))

	tracker := &readToolFileTracker{}
	tool := newReadToolForTest(workingDir, tracker)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: "internal/ui/dialog/render.go"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "File not found: internal/ui/dialog/render.go")
	require.Contains(t, resp.Content, "Use glob pattern: **/render.go")
	require.Contains(t, resp.Content, "Read parent directory: internal/ui/dialog")
	require.Contains(t, resp.Content, "Search symbol/content with grep before retrying read")
	require.Empty(t, tracker.reads)

	meta := parseReadMetadata(t, resp)
	require.Equal(t, "internal/ui/dialog/render.go", meta.MissingPath)
	require.Equal(t, []string{"**/render.go"}, meta.SuggestedGlobs)
	require.Equal(t, []string{"internal/ui/dialog"}, meta.SuggestedParentDirs)
	require.NotNil(t, meta.RecoveryAvailable)
	require.False(t, *meta.RecoveryAvailable)
	require.Equal(t, "file_not_found_suggestions", meta.RecoveredBy)
}

func TestReadToolDoesNotRecoverAmbiguousSuffixMatch(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	for _, rel := range []string{
		filepath.Join("internal", "ui", "dialog", "render.go"),
		filepath.Join("pkg", "render.go"),
	} {
		path := filepath.Join(workingDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("package test\n"), 0o644))
	}

	tracker := &readToolFileTracker{}
	tool := newReadToolForTest(workingDir, tracker)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: "render.go"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "File not found: render.go")
	require.Contains(t, resp.Content, "Use glob pattern: **/render.go")
	require.Empty(t, tracker.reads)

	meta := parseReadMetadata(t, resp)
	require.Equal(t, "render.go", meta.MissingPath)
	require.NotEqual(t, "unique_suffix_recovery", meta.RecoveredBy)
	require.NotNil(t, meta.RecoveryAvailable)
	require.False(t, *meta.RecoveryAvailable)
}

func TestFileDiscoveryToolDescriptionsDiscourageGuessedReadPaths(t *testing.T) {
	t.Parallel()

	require.Contains(t, string(readDescription), "Do not use read to probe guessed paths")
	require.Contains(t, string(readDescription), "follow the returned glob, parent-directory, or grep suggestion")
	require.Contains(t, string(globDescription), "Use Glob for uncertain file-name lookups before read")
	require.Contains(t, string(grepDescription), "Use Grep to locate symbols or content before reading")
}

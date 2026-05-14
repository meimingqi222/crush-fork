package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type inMemoryFileTracker struct {
	mu    sync.Mutex
	reads map[string]time.Time
}

func newHashlineEditFileTracker() *inMemoryFileTracker {
	return &inMemoryFileTracker{reads: make(map[string]time.Time)}
}

func (m *inMemoryFileTracker) RecordRead(_ context.Context, sessionID, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads[sessionID+":"+path] = time.Now()
}

func (m *inMemoryFileTracker) LastReadTime(_ context.Context, sessionID, path string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads[sessionID+":"+path]
}

func (m *inMemoryFileTracker) ListReadFiles(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestReadTextFileBoundaryCases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")

	var allLines []string
	for i := range 5 {
		allLines = append(allLines, fmt.Sprintf("line %d", i+1))
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(allLines, "\n")), 0o644))

	tests := []struct {
		name        string
		offset      int
		limit       int
		wantContent string
		wantHasMore bool
	}{
		{
			name:        "exactly limit lines remaining",
			offset:      0,
			limit:       5,
			wantContent: "line 1\nline 2\nline 3\nline 4\nline 5",
			wantHasMore: false,
		},
		{
			name:        "limit plus one line remaining",
			offset:      0,
			limit:       4,
			wantContent: "line 1\nline 2\nline 3\nline 4",
			wantHasMore: true,
		},
		{
			name:        "offset at last line",
			offset:      4,
			limit:       3,
			wantContent: "line 5",
			wantHasMore: false,
		},
		{
			name:        "offset beyond eof",
			offset:      10,
			limit:       3,
			wantContent: "",
			wantHasMore: false,
		},
		{
			name:        "offset at eof",
			offset:      5,
			limit:       3,
			wantContent: "",
			wantHasMore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotContent, gotHasMore, err := readTextFile(filePath, tt.offset, tt.limit)
			if tt.offset > len(allLines) {
				require.ErrorIs(t, err, errViewOffsetBeyondEOF)
				require.Equal(t, "", gotContent)
				require.False(t, gotHasMore)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantContent, gotContent)
			require.Equal(t, tt.wantHasMore, gotHasMore)
		})
	}
}

func TestReadTextFileTruncatesLongLines(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "longline.txt")

	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("a", MaxLineLength)+"...", content)
}

func TestReadTextFileLinesPreservesRawLongLinesForHashline(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "longline.txt")

	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine), 0o644))

	result, err := readTextFileLines(filePath, 0, 1)
	require.NoError(t, err)
	require.False(t, result.HasMore)
	require.Len(t, result.Lines, 1)
	require.Equal(t, longLine, result.Lines[0].Raw)
	require.Equal(t, strings.Repeat("a", MaxLineLength)+"...", result.Lines[0].Display)
}

func TestViewHashlineSupportsEditingTruncatedLongLines(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "main.txt")
	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine+"\n"), 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tracker := newHashlineEditFileTracker()
	viewTool := NewViewTool(nil, permissions, tracker, workingDir, config.ToolLs{})
	hashlineTool := NewEditTool(nil, permissions, &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}, tracker, workingDir)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	viewResp, err := runViewTool(t, viewTool, ctx, ViewParams{FilePath: "main.txt", Hashline: true})
	require.NoError(t, err)
	require.False(t, viewResp.IsError)
	require.Contains(t, viewResp.Content, strings.Repeat("a", MaxLineLength)+"...")

	lineRef := formatHashlineReference(1, longLine)
	require.Contains(t, viewResp.Content, lineRef)

	editResp, err := runEditTool(t, hashlineTool, ctx, EditParams{
		FilePath: filePath,
		Operations: []HashlineEditOperation{
			{
				Operation: hashlineEditOpReplaceLine,
				Line:      lineRef,
				Content:   "short line",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, editResp.IsError, editResp.Content)

	updated, readErr := os.ReadFile(filePath)
	require.NoError(t, readErr)
	require.Equal(t, "short line\n", string(updated))
}

func TestViewTool_AllowsLargeImageWithCompression(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "large.png")
	largePNG := createLargePNG(t)
	require.Greater(t, len(largePNG), MaxViewSize)
	require.NoError(t, os.WriteFile(filePath, largePNG, 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	ctx = context.WithValue(ctx, ModelNameContextKey, "test-model")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "large.png"})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.NotContains(t, resp.Content, "File is too large")
}

func TestViewTool_AllowsReadingLargeFilesWithLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "large.txt")
	largeText := bytes.Repeat([]byte("a\n"), 10000) // 10000 lines
	require.NoError(t, os.WriteFile(filePath, largeText, 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// Test that large files can be read with a limit parameter
	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "large.txt", Limit: 10})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "a") // Should contain some content
}

func TestViewTool_IncludesPreciseContinuationOffset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\nfour"), 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "sample.txt", Offset: 1, Limit: 2})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     2|two")
	require.Contains(t, resp.Content, "     3|three")
	require.Contains(t, resp.Content, "(Showing lines 2-3. Use offset=3 to continue.)")
}

func TestViewTool_IncludesEndOfFileTotal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree"), 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "sample.txt", Offset: 1, Limit: 5})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     2|two")
	require.Contains(t, resp.Content, "     3|three")
	require.Contains(t, resp.Content, "(Showing lines 2-3. End of file - total 3 lines.)")
}

func TestViewTool_OffsetBeyondEndIncludesTotalLines(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree"), 0o644))

	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "sample.txt", Offset: 10, Limit: 5})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Offset 10 is beyond end of file (3 lines)")
}

func TestViewTool_MissingFileUsesConciseError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "missing.txt"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "File not found")
	require.NotContains(t, resp.Content, "Use glob")
	require.NotContains(t, resp.Metadata, "file_not_found_recovery")
	require.NotContains(t, resp.Metadata, "glob")
}

func TestViewTool_MissingFileIncludesSimilarPathSuggestions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "read.go.txt"), []byte("package test"), 0o644))
	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "read.go"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Did you mean one of these?")
	require.Contains(t, resp.Content, "read.go.txt")
	require.Contains(t, resp.Metadata, "file_not_found_suggestions")
}

func TestViewTool_RecoversMissingExtensionToUniqueDirectory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, "internal", "agent", "tools")
	require.NoError(t, os.MkdirAll(toolsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "view.go"), []byte("package tools"), 0o644))
	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "internal/agent/tools.go"})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "view.go")

	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, toolsDir, meta.FilePath)
	require.Equal(t, "directory_extension_recovery", meta.RecoveredBy)
	require.Contains(t, meta.RecoveryAction, "internal/agent/tools.go")
	require.True(t, meta.IsDirectory)
}

func TestViewTool_MissingAmbiguousSuffixDoesNotRecover(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "internal", "agent"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "one", "internal", "agent"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "two", "internal", "agent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "internal", "agent", "tools.go.txt"), []byte("package tools"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "one", "internal", "agent", "tools.go"), []byte("package tools"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "two", "internal", "agent", "tools.go"), []byte("package tools"), 0o644))
	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "internal/agent/tools.go"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "File not found")
	require.Contains(t, resp.Content, "Did you mean one of these?")
	require.Contains(t, resp.Content, "tools.go.txt")
	require.Contains(t, resp.Metadata, "file_not_found_suggestions")
	require.NotContains(t, resp.Metadata, "unique_suffix_recovery")
}

func TestViewTool_InvalidPathSyntaxReturnsToolErrorResponse(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "windows" {
		t.Skip("windows-specific invalid path syntax behavior")
	}

	tmpDir := t.TempDir()
	permissions := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, &mockFileTracker{}, tmpDir, config.ToolLs{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp, err := runViewTool(t, tool, ctx, ViewParams{FilePath: "*.go"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "error accessing file")
}

func TestShouldWaitForDiagnostics_DefaultsToTrue(t *testing.T) {
	t.Parallel()

	require.True(t, shouldWaitForDiagnostics(nil))
}

func TestShouldWaitForDiagnostics_UsesExplicitValue(t *testing.T) {
	t.Parallel()

	wait := true
	skip := false

	require.True(t, shouldWaitForDiagnostics(&wait))
	require.False(t, shouldWaitForDiagnostics(&skip))
}

func runViewTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ViewParams) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	return tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  ViewToolName,
		Input: string(input),
	})
}

func runEditTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params EditParams) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	return tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  EditToolName,
		Input: string(input),
	})
}

func createLargePNG(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	_, err := rand.Read(img.Pix)
	require.NoError(t, err)

	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	return buf.Bytes()
}

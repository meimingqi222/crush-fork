package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestReadToolReadsArchiveReferenceWithSelectors(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	archiveDir := t.TempDir()
	archiveID := "aabbccddeeff00112233445566778899"
	archivePath := filepath.Join(archiveDir, archiveID+".txt")
	require.NoError(t, os.WriteFile(archivePath, []byte("one\ntwo\nthree\n"), 0o644))

	tool := NewReadToolWithArchiveDir(nil, nil, &readToolFileTracker{}, workingDir, config.ToolLs{}, nil, archiveDir, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://aabbccddeeff:2-2"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "two")
	require.NotContains(t, resp.Content, "one")

	raw := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://aabbccddeeff:raw"})
	require.False(t, raw.IsError)
	require.Equal(t, "one\ntwo\nthree\n", raw.Content)
}

func TestReadToolRejectsInvalidArchiveReference(t *testing.T) {
	t.Parallel()

	tool := NewReadToolWithArchiveDir(nil, nil, nil, t.TempDir(), config.ToolLs{}, nil, t.TempDir(), nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	for _, path := range []string{
		"archive://../secret",
		"archive://aabbccddeeff/../secret",
		"archive://aabbccddeeff?x=1",
	} {
		resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: path})
		require.True(t, resp.IsError, path)
		require.Contains(t, resp.Content, "invalid archive reference", path)
	}
}

func TestReadToolReportsMissingAndAmbiguousArchiveReferences(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	archiveDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "aabbccddeeff00112233445566778899.txt"), []byte("one"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "aabbccddeeffffeeddccbbaa99887766.txt"), []byte("two"), 0o644))
	tool := NewReadToolWithArchiveDir(nil, nil, nil, workingDir, config.ToolLs{}, nil, archiveDir, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	missing := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://deadbeef"})
	require.True(t, missing.IsError)
	require.Contains(t, missing.Content, "archive not found")

	ambiguous := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://aabbccddeeff"})
	require.True(t, ambiguous.IsError)
	require.Contains(t, ambiguous.Content, "archive reference is ambiguous")
}

func TestReadToolRawArchiveTruncationHintsContinuation(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	archiveDir := t.TempDir()
	archiveID := "11223344556677889900aabbccddeeff"

	// More lines than DefaultReadLimit (2000).
	var buf strings.Builder
	for i := 1; i <= DefaultReadLimit+5; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, archiveID+".txt"), []byte(buf.String()), 0o644))

	tool := NewReadToolWithArchiveDir(nil, nil, &readToolFileTracker{}, workingDir, config.ToolLs{}, nil, archiveDir, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	// Raw mode must not silently truncate — it should point at the next page.
	resp := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://11223344556677:raw"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "line 1")
	require.Contains(t, resp.Content, "Truncated after line "+strconv.Itoa(DefaultReadLimit))
	require.Contains(t, resp.Content, "archive://11223344556677:"+strconv.Itoa(DefaultReadLimit+1))

	// Small archive in raw mode: no truncation hint.
	smallPath := filepath.Join(archiveDir, "ffee0000000000000000000000000000.txt")
	require.NoError(t, os.WriteFile(smallPath, []byte("a\nb\nc\n"), 0o644))
	respSmall := runReadToolForTest(t, tool, ctx, ReadParams{Path: "archive://ffee000000000000:raw"})
	require.False(t, respSmall.IsError)
	require.Equal(t, "a\nb\nc\n", respSmall.Content)
	require.NotContains(t, respSmall.Content, "Truncated")
}

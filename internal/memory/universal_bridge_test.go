package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUniversalBridgeExport(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, svc.Store(ctx, StoreParams{
		Key:   "preferred-language",
		Value: "User prefers Go over TypeScript for backend work.",
		Type:  "preference",
		Scope: "global",
		Tags:  []string{"language", "backend"},
	}))
	require.NoError(t, svc.Store(ctx, StoreParams{
		Key:   "mono-repo-decision",
		Value: "Decided to use a monorepo layout in April 2026.",
		Type:  "decision",
		Scope: "project",
	}))

	bridge := NewUniversalBridge(svc)
	records, err := bridge.Export(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)

	titles := make(map[string]string, 2)
	for _, r := range records {
		titles[r.Title] = r.Kind
	}
	require.Equal(t, "preference", titles["preferred-language"])
	require.Equal(t, "decision", titles["mono-repo-decision"])
}

func TestUniversalBridgeExportScope(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, svc.Store(ctx, StoreParams{Key: "global-fact", Value: "v1", Scope: "global"}))
	require.NoError(t, svc.Store(ctx, StoreParams{Key: "project-fact", Value: "v2", Scope: "project"}))

	bridge := NewUniversalBridge(svc)
	records, err := bridge.ExportScope(ctx, "global")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "global-fact", records[0].Title)
	require.Equal(t, "global", records[0].Scope)
}

func TestUniversalBridgeImport(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	bridge := NewUniversalBridge(svc)

	r := UniversalRecord{
		ID:      "abc123",
		Kind:    "workflow",
		Title:   "release-process",
		Content: "1. Run tests\n2. Tag release\n3. Deploy",
		Summary: "Standard release workflow",
		Scope:   "project",
		Tags:    []string{"ci", "release"},
		Agent:   "opencode",
		Origin:  "auto_extract",
	}
	require.NoError(t, bridge.Import(ctx, r))

	entry, err := svc.Get(ctx, "release-process")
	require.NoError(t, err)
	require.Equal(t, "release-process", entry.Key)
	require.Equal(t, "workflow", entry.Type)
	require.Equal(t, "project", entry.Scope)
	require.True(t, strings.Contains(entry.Value, "Run tests"))
	require.Equal(t, []string{"ci", "release"}, entry.Tags)
}

func TestUniversalBridgeImportSkipsStale(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	bridge := NewUniversalBridge(svc)

	stale := UniversalRecord{
		Title:   "old-fact",
		Content: "This is outdated",
		Scope:   "project",
		Stale:   true,
	}
	require.NoError(t, bridge.Import(ctx, stale))

	_, err = svc.Get(ctx, "old-fact")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestUniversalBridgeImportBatch(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	bridge := NewUniversalBridge(svc)

	records := []UniversalRecord{
		{Title: "fact-a", Content: "content-a", Kind: "fact", Scope: "project"},
		{Title: "fact-b", Content: "content-b", Kind: "fact", Scope: "project"},
		{Title: "", Content: "no title", Kind: "fact", Scope: "project"}, // should error but not abort
	}
	err = bridge.ImportBatch(ctx, records)
	require.NoError(t, err) // 2 succeed, 1 fails → no "all failed" error

	entryA, err := svc.Get(ctx, "fact-a")
	require.NoError(t, err)
	require.Equal(t, "content-a", entryA.Value)

	entryB, err := svc.Get(ctx, "fact-b")
	require.NoError(t, err)
	require.Equal(t, "content-b", entryB.Value)
}

func TestUniversalBridgeRecallAsUniversal(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, svc.Store(ctx, StoreParams{
		Key:   "typescript-tooling",
		Value: "We use bun for all TypeScript builds",
		Scope: "project",
	}))
	require.NoError(t, svc.Store(ctx, StoreParams{
		Key:   "go-tooling",
		Value: "We use task runner for Go builds",
		Scope: "project",
	}))

	bridge := NewUniversalBridge(svc)
	records, err := bridge.RecallAsUniversal(ctx, "bun", "project", 10)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "typescript-tooling", records[0].Title)
}

func TestKindRoundtrip(t *testing.T) {
	t.Parallel()

	kinds := []string{"identity", "preference", "decision", "workflow", "summary", "event", "fact"}
	for _, kind := range kinds {
		crushType := universalKindToCrushType(kind)
		back := crushTypeTouniversalKind(crushType)
		require.Equal(t, kind, back, "kind roundtrip failed for %q", kind)
	}
}

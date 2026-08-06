package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockExtractorDeps holds mock callbacks used across tests.
type mockExtractorDeps struct {
	transcriptFn func(ctx context.Context, sessionID string) (Transcript, error)
	analyzeFn    func(ctx context.Context, transcript string) ([]ExtractedEvent, error)
	filesFn      func(ctx context.Context, sessionID string) []string
	clock        func() time.Time
}

func newMockExtractor(deps mockExtractorDeps) *LLMExtractor {
	return &LLMExtractor{
		getTranscript: deps.transcriptFn,
		analyzeEvents: deps.analyzeFn,
		getFiles:      deps.filesFn,
		clock:         deps.clock,
	}
}

func fixedClock() time.Time {
	return time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
}

func defaultTranscriptFn(t *testing.T, text string, msgIDs []string) func(ctx context.Context, sessionID string) (Transcript, error) {
	t.Helper()
	return func(_ context.Context, sessionID string) (Transcript, error) {
		return Transcript{Text: text, MessageIDs: msgIDs}, nil
	}
}

func defaultAnalyzeFn(t *testing.T, events []ExtractedEvent) func(ctx context.Context, transcript string) ([]ExtractedEvent, error) {
	t.Helper()
	return func(_ context.Context, transcript string) ([]ExtractedEvent, error) {
		return events, nil
	}
}

func defaultFilesFn(files []string) func(ctx context.Context, sessionID string) []string {
	return func(_ context.Context, _ string) []string {
		return files
	}
}

func TestExtractor_EmptyTranscript(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: func(_ context.Context, _ string) (Transcript, error) {
			return Transcript{}, nil
		},
		analyzeFn: func(_ context.Context, _ string) ([]ExtractedEvent, error) {
			return []ExtractedEvent{{Kind: MemoryKindDecision}}, nil
		},
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})

	events, err := ext.Extract(context.Background(), "sess-empty")
	require.NoError(t, err)
	require.Nil(t, events, "empty transcript should return nil")
}

func TestExtractor_EmptyAnalysis(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t, "some conversation", nil),
		analyzeFn:    defaultAnalyzeFn(t, nil),
		filesFn:      defaultFilesFn(nil),
		clock:        fixedClock,
	})

	events, err := ext.Extract(context.Background(), "sess-no-events")
	require.NoError(t, err)
	require.Nil(t, events, "empty analysis should return nil")
}

func TestExtractor_TranscriptError(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: func(_ context.Context, _ string) (Transcript, error) {
			return Transcript{}, errors.New("transcript fetch failed")
		},
	})
	_, err := ext.Extract(context.Background(), "sess-err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "getting transcript")
}

func TestExtractor_AnalysisError(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t, "some conversation", []string{"msg-1"}),
		analyzeFn: func(_ context.Context, _ string) ([]ExtractedEvent, error) {
			return nil, errors.New("LLM call failed")
		},
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})
	_, err := ext.Extract(context.Background(), "sess-err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "analyzing events")
}

func TestExtractor_SingleTurnExtraction(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"USER: let's use SQLite\nASSISTANT: good choice",
			[]string{"msg-1", "msg-2"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use SQLite for storage",
				Summary:    "Storage decision",
				Confidence: 0.9,
				Importance: 0.7,
				Tags:       []string{"database", "sqlite"},
			},
		}),
		filesFn: defaultFilesFn([]string{"go.mod", "main.go"}),
		clock:   fixedClock,
	})

	events, err := ext.Extract(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	require.Equal(t, MemoryKindDecision, evt.Kind)
	require.Equal(t, MemoryScopeProject, evt.Scope)
	require.Equal(t, "Use SQLite for storage", evt.Content)
	require.Equal(t, "Storage decision", evt.Summary)
	require.Equal(t, 0.924, evt.Confidence) // BayesianUpdate(0.9, "stated") = 0.9 + (1-0.9)*0.8*0.3 = 0.924
	require.Equal(t, 0.7, evt.Importance)
	require.Equal(t, []string{"database", "sqlite"}, evt.Tags)
	require.Equal(t, fixedClock(), evt.CreatedAt)
	require.Equal(t, fixedClock(), evt.UpdatedAt)

	// Provenance
	require.Equal(t, "sess-1", evt.Source.SessionID)
	require.Equal(t, []string{"msg-1", "msg-2"}, evt.Source.MessageIDs)
	require.Equal(t, []string{"go.mod", "main.go"}, evt.Source.Files)

	// Event ID format
	require.Contains(t, evt.ID, "ext-sess-1-")
}

func TestExtractor_MultiTurnExtraction(t *testing.T) {
	t.Parallel()
	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"longer conversation with multiple events",
			[]string{"msg-1", "msg-2", "msg-3"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use SQLite",
				Summary:    "Decision",
				Confidence: 0.9,
				Importance: 0.7,
			},
			{
				Kind:       MemoryKindPreference,
				Scope:      MemoryScopeUser,
				Content:    "User prefers concise code",
				Summary:    "Preference",
				Confidence: 0.8,
				Importance: 0.5,
				Tags:       []string{"style"},
			},
			{
				Kind:       MemoryKindPitfall,
				Scope:      MemoryScopeProject,
				Content:    "Avoid global state in handlers",
				Summary:    "Gotcha",
				Confidence: 0.7,
				Importance: 0.9,
			},
		}),
		filesFn: defaultFilesFn([]string{"handler.go"}),
		clock:   fixedClock,
	})

	events, err := ext.Extract(context.Background(), "sess-multi")
	require.NoError(t, err)
	require.Len(t, events, 3)

	// Verify event types
	require.Equal(t, MemoryKindDecision, events[0].Kind)
	require.Equal(t, MemoryKindPreference, events[1].Kind)
	require.Equal(t, MemoryKindPitfall, events[2].Kind)

	// Each event gets a unique ID
	ids := make(map[string]bool)
	for _, e := range events {
		ids[e.ID] = true
	}
	require.Len(t, ids, 3, "each event must have a unique ID")

	// All events share the same provenance
	for _, e := range events {
		require.Equal(t, "sess-multi", e.Source.SessionID)
		require.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, e.Source.MessageIDs)
		require.Equal(t, []string{"handler.go"}, e.Source.Files)
	}
}

func TestExtractor_EventTypes(t *testing.T) {
	t.Parallel()
	allKinds := []struct {
		kind  MemoryKind
		scope MemoryScope
	}{
		{MemoryKindDecision, MemoryScopeProject},
		{MemoryKindPreference, MemoryScopeUser},
		{MemoryKindProcedure, MemoryScopeProject},
		{MemoryKindPitfall, MemoryScopeProject},
		{MemoryKindReference, MemoryScopeProject},
		{MemoryKindTaskState, MemoryScopeSession},
		{MemoryKindWorkingMemory, MemoryScopeSession},
	}

	events := make([]ExtractedEvent, len(allKinds))
	for i, k := range allKinds {
		events[i] = ExtractedEvent{
			Kind:       k.kind,
			Scope:      k.scope,
			Content:    string(k.kind) + " content",
			Summary:    string(k.kind),
			Confidence: 0.8,
			Importance: 0.6,
		}
	}

	ext := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t, "conversation with all event types", nil),
		analyzeFn:    defaultAnalyzeFn(t, events),
		filesFn:      defaultFilesFn(nil),
		clock:        fixedClock,
	})

	result, err := ext.Extract(context.Background(), "sess-types")
	require.NoError(t, err)
	require.Len(t, result, len(allKinds))
	for i, k := range allKinds {
		require.Equal(t, k.kind, result[i].Kind, "index %d kind mismatch", i)
		require.Equal(t, k.scope, result[i].Scope, "index %d scope mismatch", i)
	}
}

func TestEngine_AfterTurnIdleWithExtractor(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"USER: use postgres\nASSISTANT: ok",
			[]string{"msg-1"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "Use Postgres", Confidence: 0.9, Importance: 0.8},
		}),
		filesFn: defaultFilesFn([]string{"db.go"}),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	ctx := context.Background()

	// First call: extractor should produce events
	err := eng.AfterTurnIdle(ctx, "sess-ext-test", nil)
	require.NoError(t, err)

	// Verify events were stored
	scope := MemoryScopeProject
	events, err := eng.store.Query(ctx, EventFilter{Scope: &scope})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "Use Postgres", events[0].Content)
	require.Equal(t, "sess-ext-test", events[0].Source.SessionID)
	require.NotNil(t, eng.lastExtractionRun)

	// Verify extraction timestamp recorded
	require.NotNil(t, eng.lastExtractionRun)
	require.False(t, eng.lastExtractionRun.IsZero())
}

func TestEngine_AfterTurnIdleMaterializesExtractedEvents(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	dir := t.TempDir()
	writer := NewArtifactWriter(dir)
	eng.SetMaterializer(NewSummaryMaterializer(db, eng.store, writer))
	eng.SetMaterializer(NewMemoryMDMaterializer(db, eng.store, writer))

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"USER: use postgres\nASSISTANT: ok",
			[]string{"msg-1"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use Postgres for persistence.",
				Summary:    "Use Postgres",
				Confidence: 0.9,
				Importance: 0.8,
			},
		}),
		filesFn: defaultFilesFn([]string{"db.go"}),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	err := eng.AfterTurnIdle(context.Background(), "sess-ext-materialize", nil)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err, "turn-end extraction should refresh memory_summary.md")
	_, err = os.Stat(filepath.Join(dir, "MEMORY.md"))
	require.NoError(t, err, "turn-end extraction should refresh MEMORY.md")

	views, err := eng.queryViewStatuses(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 2)
	for _, view := range views {
		require.Equal(t, int64(1), view.Watermark)
	}
}

func TestEngine_AfterTurnIdleSkipsMaterializationForSessionEvents(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	dir := t.TempDir()
	writer := NewArtifactWriter(dir)
	eng.SetMaterializer(NewSummaryMaterializer(db, eng.store, writer))
	eng.SetMaterializer(NewMemoryMDMaterializer(db, eng.store, writer))

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"USER: continue current task\nASSISTANT: ok",
			[]string{"msg-1"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{
				Kind:       MemoryKindTaskState,
				Scope:      MemoryScopeSession,
				Content:    "Transient task state.",
				Summary:    "Transient task state",
				Confidence: 0.9,
				Importance: 0.8,
			},
		}),
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	err := eng.AfterTurnIdle(context.Background(), "sess-session-only", nil)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.True(t, os.IsNotExist(err), "session-only events should not refresh memory_summary.md")
	_, err = os.Stat(filepath.Join(dir, "MEMORY.md"))
	require.True(t, os.IsNotExist(err), "session-only events should not refresh MEMORY.md")
}

func TestEngine_OnBeforeCompactionExtractsAndMaterializes(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	dir := t.TempDir()
	writer := NewArtifactWriter(dir)
	eng.SetMaterializer(NewSummaryMaterializer(db, eng.store, writer))
	eng.SetMaterializer(NewMemoryMDMaterializer(db, eng.store, writer))

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t,
			"USER: remember this before compaction\nASSISTANT: ok",
			[]string{"msg-1"},
		),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{
				Kind:       MemoryKindPreference,
				Scope:      MemoryScopeUser,
				Content:    "User wants memory saved before compaction.",
				Summary:    "Save memory before compaction",
				Confidence: 0.9,
				Importance: 0.8,
			},
		}),
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	err := eng.OnBeforeCompaction(context.Background(), "sess-pre-compact")
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "memory_summary.md"))
	require.NoError(t, err, "pre-compaction should refresh memory_summary.md")
	_, err = os.Stat(filepath.Join(dir, "MEMORY.md"))
	require.NoError(t, err, "pre-compaction should refresh MEMORY.md")
}

func TestEngine_AfterTurnIdleWithExtractorEmptyTranscript(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: func(_ context.Context, _ string) (Transcript, error) {
			return Transcript{}, nil
		},
		analyzeFn: func(_ context.Context, _ string) ([]ExtractedEvent, error) {
			return []ExtractedEvent{{Kind: MemoryKindDecision}}, nil
		},
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	// Empty transcript → no events produced → nothing stored
	err := eng.AfterTurnIdle(context.Background(), "sess-empty", nil)
	require.NoError(t, err)

	allEvents, err := eng.store.Query(context.Background(), EventFilter{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, allEvents)
}

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/stretchr/testify/require"
)

// mockEventStore implements engine.EventStore for testing.
type mockEventStore struct {
	appended []engine.MemoryEvent
	queryFn  func(ctx context.Context, filter engine.EventFilter) ([]engine.MemoryEvent, error)
}

func (m *mockEventStore) Append(_ context.Context, event engine.MemoryEvent) error {
	m.appended = append(m.appended, event)
	return nil
}

func (m *mockEventStore) Query(ctx context.Context, filter engine.EventFilter) ([]engine.MemoryEvent, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, filter)
	}
	return nil, nil
}

func (m *mockEventStore) Count(ctx context.Context, filter engine.EventFilter) (int64, error) {
	events, err := m.Query(ctx, filter)
	return int64(len(events)), err
}

func (m *mockEventStore) GetByID(_ context.Context, _ string) (*engine.MemoryEvent, error) {
	return nil, nil
}

func (m *mockEventStore) GetMaxWatermark(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockEventStore) RecentSessions(_ context.Context, _ int64, _ int) ([]string, error) {
	return nil, nil
}

func (m *mockEventStore) Close() error {
	return nil
}

// mockRetriever implements engine.Retriever for testing.
type mockRetriever struct {
	recallResult   string
	recallErr      error
	retrieveResult []engine.MemoryEvent
	retrieveErr    error
	reflectResult  string
	reflectErr     error
}

func (m *mockRetriever) Recall(_ context.Context, _ map[string]any) (string, error) {
	return m.recallResult, m.recallErr
}

func (m *mockRetriever) Retrieve(_ context.Context, _ string, _ map[string]any) ([]engine.MemoryEvent, error) {
	return m.retrieveResult, m.retrieveErr
}

func (m *mockRetriever) Reflect(_ context.Context, _ string, _ map[string]any) (string, error) {
	return m.reflectResult, m.reflectErr
}

// mockEngine is a minimal memoryMaterializer for testing retain's
// post-write materialization trigger.
type mockEngine struct {
	materializationRuns int
}

func (m *mockEngine) TriggerMaterialization(context.Context) error {
	m.materializationRuns++
	return nil
}

// mockBackendStatus implements BackendStatusProvider for testing
// memory_status, which now reports the simplified memory.Status rather than
// the full engine.EngineStatus pipeline diagnostics.
type mockBackendStatus struct {
	status    *memory.Status
	statusErr error
}

func (m *mockBackendStatus) Status(ctx context.Context) (*memory.Status, error) {
	return m.status, m.statusErr
}

// --- retain tests ---

func TestRetainTool(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	materializer := &mockEngine{}
	tool := NewRetainTool(eventStore, "/workspace", materializer)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRetainTool(t, tool, ctx, RetainParams{
		Scope:      "project",
		Kind:       "decision",
		Content:    "Use Go 1.22 for the project",
		Summary:    "Go version decision",
		Tags:       []string{"go", "tech-stack"},
		Importance: 0.8,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Retained memory event [project/decision]")
	require.Len(t, eventStore.appended, 1)

	evt := eventStore.appended[0]
	require.Equal(t, engine.MemoryScope("project"), evt.Scope)
	require.Equal(t, engine.MemoryKind("decision"), evt.Kind)
	require.Equal(t, "Use Go 1.22 for the project", evt.Content)
	require.Equal(t, "Go version decision", evt.Summary)
	require.Equal(t, float64(0.8), evt.Importance)
	require.Equal(t, []string{"go", "tech-stack"}, evt.Tags)
	require.Equal(t, "session-1", evt.Source.SessionID)
	require.NotEmpty(t, evt.ID)
	require.Equal(t, 1, materializer.materializationRuns)
}

func TestRetainToolRequiresSession(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	tool := NewRetainTool(eventStore, "/workspace")

	_, err := runRetainTool(t, tool, context.Background(), RetainParams{
		Scope:   "project",
		Kind:    "decision",
		Content: "test",
	})
	require.ErrorContains(t, err, "session ID is required")
}

func TestRetainToolNilEventStore(t *testing.T) {
	t.Parallel()

	tool := NewRetainTool(nil, "/workspace")
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRetainTool(t, tool, ctx, RetainParams{
		Scope:   "project",
		Kind:    "decision",
		Content: "test",
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Memory engine is not available")
}

func TestRetainToolDefaultImportance(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	tool := NewRetainTool(eventStore, "/workspace")
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	_, err := runRetainTool(t, tool, ctx, RetainParams{
		Scope:   "session",
		Kind:    "preference",
		Content: "test",
	})
	require.NoError(t, err)
	require.Len(t, eventStore.appended, 1)
	require.Equal(t, float64(0.5), eventStore.appended[0].Importance)
}

func TestRetainToolSkipsMaterializationForSessionMemory(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	materializer := &mockEngine{}
	tool := NewRetainTool(eventStore, "/workspace", materializer)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	_, err := runRetainTool(t, tool, ctx, RetainParams{
		Scope:   "session",
		Kind:    "task_state",
		Content: "temporary task state",
	})
	require.NoError(t, err)
	require.Len(t, eventStore.appended, 1)
	require.Equal(t, 0, materializer.materializationRuns)
}

// --- recall tests ---

func TestRecallToolBasic(t *testing.T) {
	t.Parallel()

	retriever := &mockRetriever{
		recallResult: "## Memory Summary\n\n- User prefers Go\n- Project uses SQLite",
	}
	tool := NewRecallTool(retriever, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRecallTool(t, tool, ctx, RecallParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "User prefers Go")
	require.Contains(t, resp.Content, "Project uses SQLite")
}

func TestRecallToolWithQuery(t *testing.T) {
	t.Parallel()

	events := []engine.MemoryEvent{
		{
			ID:         "evt-1",
			Scope:      engine.MemoryScopeProject,
			Kind:       engine.MemoryKindDecision,
			Content:    "Use SQLite for persistence",
			Summary:    "Database decision",
			Source:     engine.MemorySourceRef{SessionID: "sess-1"},
			Confidence: 0.9,
			Importance: 0.8,
			Tags:       []string{"database", "sqlite"},
		},
	}
	retriever := &mockRetriever{
		retrieveResult: events,
	}
	tool := NewRecallTool(retriever, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRecallTool(t, tool, ctx, RecallParams{
		Query: "database",
		Scope: "project",
		Kind:  "decision",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "[project/decision]")
	require.Contains(t, resp.Content, "Database decision")
	require.Contains(t, resp.Content, "SQLite for persistence")
}

func TestRecallToolNilRetriever(t *testing.T) {
	t.Parallel()

	tool := NewRecallTool(nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRecallTool(t, tool, ctx, RecallParams{Query: "test"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Memory engine retriever is not available")
}

func TestRecallToolEmptyResult(t *testing.T) {
	t.Parallel()

	retriever := &mockRetriever{recallResult: ""}
	tool := NewRecallTool(retriever, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRecallTool(t, tool, ctx, RecallParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No materialized memory available")
}

func TestRecallToolQueryEmptyResult(t *testing.T) {
	t.Parallel()

	retriever := &mockRetriever{}
	tool := NewRecallTool(retriever, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runRecallTool(t, tool, ctx, RecallParams{Query: "nonexistent"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No matching memory events found")
}

// --- reflect tests ---

func TestReflectTool(t *testing.T) {
	t.Parallel()

	retriever := &mockRetriever{
		reflectResult: "Memory synthesis for: why did we choose SQLite?\n\n- [project/decision] Database decision (session: sess-1)",
	}
	tool := NewReflectTool(retriever)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")

	resp, err := runReflectTool(t, tool, ctx, ReflectParams{
		Query: "why did we choose SQLite",
		Scope: "project",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "why did we choose SQLite")
	require.Contains(t, resp.Content, "Database decision")
}

func TestReflectToolNilRetriever(t *testing.T) {
	t.Parallel()

	tool := NewReflectTool(nil)

	resp, err := runReflectTool(t, tool, context.Background(), ReflectParams{Query: "test"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Memory engine retriever is not available")
}

func TestReflectToolEmptyResult(t *testing.T) {
	t.Parallel()

	retriever := &mockRetriever{reflectResult: ""}
	tool := NewReflectTool(retriever)

	resp, err := runReflectTool(t, tool, context.Background(), ReflectParams{Query: "test"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No relevant memories found")
}

// --- memory_status tests ---

func TestMemoryStatusTool(t *testing.T) {
	t.Parallel()

	backend := &mockBackendStatus{
		status: &memory.Status{
			Backend:           "local",
			Enabled:           true,
			EventCount:        42,
			LastConsolidation: time.Now().Unix(),
		},
	}
	tool := NewMemoryStatusTool(nil)
	toolWithBackend := NewMemoryStatusTool(backend)

	t.Run("nil backend returns error", func(t *testing.T) {
		resp, err := runMemoryStatusTool(t, tool, context.Background(), MemoryStatusParams{})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "not configured")
	})

	t.Run("summary output", func(t *testing.T) {
		resp, err := runMemoryStatusTool(t, toolWithBackend, context.Background(), MemoryStatusParams{})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "backend=local")
		require.Contains(t, resp.Content, "enabled=true")
		require.Contains(t, resp.Content, "events=42")
	})
}

func TestMemoryStatusToolDegradedMode(t *testing.T) {
	t.Parallel()

	backend := &mockBackendStatus{
		status: &memory.Status{
			Backend:        "hindsight",
			Enabled:        true,
			Degraded:       true,
			DegradedReason: "hindsight backend configured without memory.remote",
		},
	}
	tool := NewMemoryStatusTool(backend)

	resp, err := runMemoryStatusTool(t, tool, context.Background(), MemoryStatusParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "degraded=true")
	require.Contains(t, resp.Content, "hindsight backend configured without memory.remote")
}

// --- helpers ---

func runRetainTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params RetainParams) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: RetainToolName, Input: string(input)})
}

func runRecallTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params RecallParams) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: RecallToolName, Input: string(input)})
}

func runReflectTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ReflectParams) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: ReflectToolName, Input: string(input)})
}

func runMemoryStatusTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params MemoryStatusParams) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: MemoryStatusToolName, Input: string(input)})
}

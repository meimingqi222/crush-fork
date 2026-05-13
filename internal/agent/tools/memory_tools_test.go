package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
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

func (m *mockEventStore) GetByID(_ context.Context, _ string) (*engine.MemoryEvent, error) {
	return nil, nil
}

func (m *mockEventStore) GetMaxWatermark(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockEventStore) Close() error {
	return nil
}

// mockRetriever implements engine.Retriever for testing.
type mockRetriever struct {
	recallResult  string
	recallErr     error
	retrieveResult []engine.MemoryEvent
	retrieveErr    error
	reflectResult string
	reflectErr    error
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

// mockEngine wraps engine.Engine for testing memory_status.
type mockEngine struct {
	status *engine.EngineStatus
	statusErr error
}

func (m *mockEngine) Status(ctx context.Context) (*engine.EngineStatus, error) {
	return m.status, m.statusErr
}

// --- retain tests ---

func TestRetainTool(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	permissions := &memoryPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true}
	tool := NewRetainTool(eventStore, permissions, "/workspace")
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
}

func TestRetainToolRequiresSession(t *testing.T) {
	t.Parallel()

	eventStore := &mockEventStore{}
	permissions := &memoryPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true}
	tool := NewRetainTool(eventStore, permissions, "/workspace")

	_, err := runRetainTool(t, tool, context.Background(), RetainParams{
		Scope:   "project",
		Kind:    "decision",
		Content: "test",
	})
	require.ErrorContains(t, err, "session ID is required")
}

func TestRetainToolNilEventStore(t *testing.T) {
	t.Parallel()

	permissions := &memoryPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true}
	tool := NewRetainTool(nil, permissions, "/workspace")
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
	permissions := &memoryPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true}
	tool := NewRetainTool(eventStore, permissions, "/workspace")
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
			ID:      "evt-1",
			Scope:   engine.MemoryScopeProject,
			Kind:    engine.MemoryKindDecision,
			Content: "Use SQLite for persistence",
			Summary: "Database decision",
			Source:  engine.MemorySourceRef{SessionID: "sess-1"},
			Confidence: 0.9,
			Importance: 0.8,
			Tags:    []string{"database", "sqlite"},
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

	now := time.Now()
	engine := &mockEngine{
		status: &engine.EngineStatus{
			EventStoreStatus: "ok",
			ExtractionStatus: engine.MemoryPipelineStatus{
				State:    "completed",
				LastRunAt: &now,
			},
			ConsolidationStatus: engine.MemoryPipelineStatus{
				State:         "completed",
				LastRunAt:     &now,
				LastWatermark: 42,
			},
			MaterializationViews: []engine.MaterializedViewStatus{
				{ViewName: "memory_summary", Watermark: 42, SchemaVersion: 1, State: "ok", LastUpdatedAt: &now},
				{ViewName: "MEMORY", Watermark: 42, SchemaVersion: 1, State: "ok", LastUpdatedAt: &now},
			},
		},
	}
	tool := NewMemoryStatusTool(nil)
	toolWithEngine := NewMemoryStatusTool(engine)

	t.Run("nil engine returns error", func(t *testing.T) {
		resp, err := runMemoryStatusTool(t, tool, context.Background(), MemoryStatusParams{})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "not configured")
	})

	t.Run("full status output", func(t *testing.T) {
		resp, err := runMemoryStatusTool(t, toolWithEngine, context.Background(), MemoryStatusParams{})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "Event Store: ok")
		require.Contains(t, resp.Content, "Extraction: completed")
		require.Contains(t, resp.Content, "Consolidation: completed")
		require.Contains(t, resp.Content, "memory_summary")
		require.Contains(t, resp.Content, "MEMORY")
	})

	t.Run("view filter narrows output", func(t *testing.T) {
		resp, err := runMemoryStatusTool(t, toolWithEngine, context.Background(), MemoryStatusParams{ViewName: "memory_summary"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "memory_summary")
		require.NotContains(t, resp.Content, "MEMORY")
	})
}

func TestMemoryStatusToolDegradedMode(t *testing.T) {
	t.Parallel()

	engine := &mockEngine{
		status: &engine.EngineStatus{
			EventStoreStatus: "ok",
			DegradedMode: &engine.DegradedModeInfo{
				Active: true,
				Reason: "Background model unavailable",
			},
		},
	}
	tool := NewMemoryStatusTool(engine)

	resp, err := runMemoryStatusTool(t, tool, context.Background(), MemoryStatusParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Degraded Mode: YES")
	require.Contains(t, resp.Content, "Background model unavailable")
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

package hindsight

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

type mockEventStore struct {
	events []engine.MemoryEvent
}

func (m *mockEventStore) Append(context.Context, engine.MemoryEvent) error {
	return nil
}

func (m *mockEventStore) Query(_ context.Context, filter engine.EventFilter) ([]engine.MemoryEvent, error) {
	var result []engine.MemoryEvent
	for _, evt := range m.events {
		if evt.Watermark > filter.MinWatermark {
			result = append(result, evt)
		}
	}
	return result, nil
}

func (m *mockEventStore) GetByID(context.Context, string) (*engine.MemoryEvent, error) {
	return nil, nil
}

func (m *mockEventStore) GetMaxWatermark(context.Context) (int64, error) {
	var maxWatermark int64
	for _, evt := range m.events {
		if evt.Watermark > maxWatermark {
			maxWatermark = evt.Watermark
		}
	}
	return maxWatermark, nil
}

func (m *mockEventStore) Close() error {
	return nil
}

func TestMaterializerReplicatesDurableEventsOnly(t *testing.T) {
	t.Parallel()

	var retained []RetainItem
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/memories", r.URL.Path)
		var req struct {
			Items []RetainItem `json:"items"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		retained = append(retained, req.Items...)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	db := setupMaterializerDB(t)
	store := &mockEventStore{events: []engine.MemoryEvent{
		{
			ID:         "durable-1",
			Scope:      engine.MemoryScopeProject,
			Kind:       engine.MemoryKindDecision,
			Content:    "Use SQLite for memory storage.",
			Summary:    "SQLite memory storage",
			Confidence: 0.9,
			Importance: 0.8,
			Watermark:  1,
			Source:     engine.MemorySourceRef{SessionID: "sess-1"},
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		},
		{
			ID:         "session-1",
			Scope:      engine.MemoryScopeSession,
			Kind:       engine.MemoryKindWorkingMemory,
			Content:    "Temporary state.",
			Confidence: 0.9,
			Watermark:  2,
		},
		{
			ID:         "low-confidence",
			Scope:      engine.MemoryScopeUser,
			Kind:       engine.MemoryKindPreference,
			Content:    "Low confidence memory.",
			Confidence: 0.2,
			Watermark:  3,
		},
	}}

	materializer := NewMaterializer(
		NewClient(server.URL, "", ""),
		db,
		store,
		WithRetainTags([]string{"project:crush-abc123"}),
	)
	require.NoError(t, materializer.Materialize(context.Background(), hindsightViewName, nil))

	require.Len(t, retained, 1)
	require.Contains(t, retained[0].Content, "SQLite memory storage")
	require.Contains(t, retained[0].Tags, "scope:project")
	require.Contains(t, retained[0].Tags, "kind:decision")
	require.Contains(t, retained[0].Tags, "project:crush-abc123")
	require.Contains(t, retained[0].Tags, "session:sess-1")
	require.Equal(t, "durable-1", retained[0].DocumentID)

	var watermark int64
	require.NoError(t, db.QueryRow(
		"SELECT watermark FROM memory_materialized_views WHERE view_name = ?",
		hindsightViewName,
	).Scan(&watermark))
	require.Equal(t, int64(3), watermark)

	require.NoError(t, materializer.Materialize(context.Background(), hindsightViewName, nil))
	require.Len(t, retained, 1, "second run should be skipped by watermark")
}

func setupMaterializerDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.Exec(`
		CREATE TABLE memory_materialized_views (
			id TEXT PRIMARY KEY,
			view_name TEXT NOT NULL UNIQUE,
			watermark INTEGER NOT NULL DEFAULT 0,
			schema_version INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
	`)
	require.NoError(t, err)
	return db
}

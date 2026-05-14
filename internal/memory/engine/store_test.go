package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	require.NoError(t, err)

	schema := `
	CREATE TABLE IF NOT EXISTS memory_events (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		kind TEXT NOT NULL,
		content TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		source_json TEXT NOT NULL DEFAULT '{}',
		source_hash TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0.5 CHECK (confidence >= 0.0 AND confidence <= 1.0),
		importance REAL NOT NULL DEFAULT 0.5 CHECK (importance >= 0.0 AND importance <= 1.0),
		supersedes TEXT,
		tags_json TEXT NOT NULL DEFAULT '[]',
		watermark INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		expires_at INTEGER,
		UNIQUE(session_id, source_hash)
	);
	CREATE INDEX IF NOT EXISTS idx_memory_events_watermark ON memory_events (watermark);
	CREATE INDEX IF NOT EXISTS idx_memory_events_scope_kind ON memory_events (scope, kind);
	CREATE INDEX IF NOT EXISTS idx_memory_events_session ON memory_events (session_id);
	CREATE INDEX IF NOT EXISTS idx_memory_events_created_at ON memory_events (created_at);

	CREATE TABLE IF NOT EXISTS memory_sources (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		source_type TEXT NOT NULL DEFAULT '',
		cursor TEXT NOT NULL DEFAULT '',
		last_processed_message_id TEXT NOT NULL DEFAULT '',
		watermark INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS memory_jobs (
		id TEXT PRIMARY KEY,
		job_type TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		owner TEXT NOT NULL DEFAULT '',
		lease_expires_at INTEGER,
		retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
		max_retries INTEGER NOT NULL DEFAULT 3 CHECK (max_retries >= 0),
		payload_json TEXT NOT NULL DEFAULT '{}',
		error_message TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS memory_materialized_views (
		id TEXT PRIMARY KEY,
		view_name TEXT NOT NULL UNIQUE,
		watermark INTEGER NOT NULL DEFAULT 0,
		schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version >= 1),
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`

	_, err = db.Exec(schema)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(dbPath)
	})

	return db
}

var eventCounter int

func testEvent(scope MemoryScope, kind MemoryKind, content string) MemoryEvent {
	eventCounter++
	id := fmt.Sprintf("evt-%d-%s-%s", eventCounter, string(scope), string(kind))
	msgID := fmt.Sprintf("msg-%d", eventCounter)
	return MemoryEvent{
		ID:      id,
		Scope:   scope,
		Kind:    kind,
		Content: content,
		Summary: "summary: " + truncate(content, 30),
		Source: MemorySourceRef{
			SessionID:  fmt.Sprintf("sess-%d", eventCounter),
			MessageIDs: []string{msgID},
			CWD:        "/test",
		},
		Confidence: 0.8,
		Importance: 0.6,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
		Tags:       []string{"test"},
	}
}

func TestEventStore_AppendAndQuery(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	evt := testEvent(MemoryScopeSession, MemoryKindDecision, "use sqlite for event store")
	err := store.Append(ctx, evt)
	require.NoError(t, err)

	events, err := store.Query(ctx, EventFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, evt.ID, events[0].ID)
	require.Equal(t, MemoryScopeSession, events[0].Scope)
	require.Equal(t, MemoryKindDecision, events[0].Kind)

	fetched, err := store.GetByID(ctx, evt.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	require.Equal(t, evt.Content, fetched.Content)
}

func TestEventStore_IdempotentAppend(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	evt := testEvent(MemoryScopeProject, MemoryKindPreference, "prefer go for backend")
	err := store.Append(ctx, evt)
	require.NoError(t, err)

	err = store.Append(ctx, evt)
	require.NoError(t, err)

	events, err := store.Query(ctx, EventFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1, "should have exactly one event after idempotent append")
}

func TestEventStore_AllowsMultipleEventsFromSameTranscript(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	source := MemorySourceRef{
		SessionID:  "sess-shared",
		MessageIDs: []string{"msg-1", "msg-2"},
		CWD:        "/test",
	}
	evt1 := testEvent(MemoryScopeProject, MemoryKindDecision, "use event sourcing")
	evt1.Source = source
	evt2 := testEvent(MemoryScopeProject, MemoryKindPitfall, "avoid dual memory writes")
	evt2.Source = source

	require.NoError(t, store.Append(ctx, evt1))
	require.NoError(t, store.Append(ctx, evt2))

	events, err := store.Query(ctx, EventFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 2, "different facts from one transcript should not collapse into one row")
}

func TestEventStore_QueryByScope(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	sessionScope := MemoryScopeSession
	projectScope := MemoryScopeProject

	evt1 := testEvent(sessionScope, MemoryKindTaskState, "task in progress")
	evt2 := testEvent(projectScope, MemoryKindDecision, "adopt event sourcing")

	err := store.Append(ctx, evt1)
	require.NoError(t, err)
	err = store.Append(ctx, evt2)
	require.NoError(t, err)

	events, err := store.Query(ctx, EventFilter{Scope: &sessionScope})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, MemoryScopeSession, events[0].Scope)
}

func TestEventStore_QueryByKind(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	pref := MemoryKindPreference
	proc := MemoryKindProcedure

	evt1 := testEvent(MemoryScopeUser, pref, "prefer dark theme")
	evt2 := testEvent(MemoryScopeProject, proc, "deploy steps")

	err := store.Append(ctx, evt1)
	require.NoError(t, err)
	err = store.Append(ctx, evt2)
	require.NoError(t, err)

	events, err := store.Query(ctx, EventFilter{Kind: &pref})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, MemoryKindPreference, events[0].Kind)
}

func TestEventStore_QueryWatermark(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	evt1 := testEvent(MemoryScopeSession, MemoryKindReference, "reference doc 1")
	evt2 := testEvent(MemoryScopeSession, MemoryKindReference, "reference doc 2")

	err := store.Append(ctx, evt1)
	require.NoError(t, err)
	err = store.Append(ctx, evt2)
	require.NoError(t, err)

	events, err := store.Query(ctx, EventFilter{MinWatermark: 0, Limit: 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, int64(1), events[0].Watermark)

	events, err = store.Query(ctx, EventFilter{MinWatermark: 1, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, int64(2), events[0].Watermark)
}

func TestEventStore_GetMaxWatermark(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	wm, err := store.GetMaxWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), wm)

	evt := testEvent(MemoryScopeGlobal, MemoryKindPitfall, "avoid global state")
	err = store.Append(ctx, evt)
	require.NoError(t, err)

	wm, err = store.GetMaxWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), wm)
}

func TestEventStore_QueryTimeRange(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	now := time.Now()

	evt1 := testEvent(MemoryScopeSession, MemoryKindTaskState, "task 1")
	evt1.CreatedAt = now.Add(-2 * time.Hour)

	evt2 := testEvent(MemoryScopeSession, MemoryKindTaskState, "task 2")
	evt2.CreatedAt = now.Add(-1 * time.Hour)

	evt3 := testEvent(MemoryScopeSession, MemoryKindTaskState, "task 3")
	evt3.CreatedAt = now

	err := store.Append(ctx, evt1)
	require.NoError(t, err)
	err = store.Append(ctx, evt2)
	require.NoError(t, err)
	err = store.Append(ctx, evt3)
	require.NoError(t, err)

	after := now.Add(-90 * time.Minute).Unix()
	before := now.Add(-30 * time.Minute).Unix()

	events, err := store.Query(ctx, EventFilter{AfterTime: &after, BeforeTime: &before})
	require.NoError(t, err)
	require.Len(t, events, 1, "should find exactly one event in range")
	require.Equal(t, evt2.ID, events[0].ID, "should find the middle event")
}

func TestEngine_Status(t *testing.T) {
	db := setupTestDB(t)
	engine := New(db, Config{Enabled: true})
	ctx := context.Background()

	status, err := engine.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, status)
	require.Equal(t, "local", status.Backend)
	require.Equal(t, "ok", status.EventStoreStatus)
	require.Empty(t, status.Jobs)
	require.Empty(t, status.MaterializationViews)
}

func TestEngine_StatusReportsBackend(t *testing.T) {
	db := setupTestDB(t)
	engine := New(db, Config{Enabled: true, Backend: "hindsight"})
	ctx := context.Background()

	status, err := engine.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "hindsight", status.Backend)
}

func TestEventStore_QueryOrderDesc(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	now := time.Now()
	evt1 := testEvent(MemoryScopeSession, MemoryKindReference, "older")
	evt1.Source.SessionID = "sess-order"
	evt1.CreatedAt = now.Add(-time.Minute)
	evt1.UpdatedAt = evt1.CreatedAt
	evt2 := testEvent(MemoryScopeSession, MemoryKindReference, "newer")
	evt2.Source.SessionID = "sess-order"
	evt2.CreatedAt = now
	evt2.UpdatedAt = now
	require.NoError(t, store.Append(ctx, evt1))
	require.NoError(t, store.Append(ctx, evt2))

	events, err := store.Query(ctx, EventFilter{
		SessionID: &evt1.Source.SessionID,
		Limit:     1,
		OrderDesc: true,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, evt2.Content, events[0].Content)
}

func TestEngine_Disabled(t *testing.T) {
	db := setupTestDB(t)
	engine := New(db, Config{Enabled: false})
	require.False(t, engine.Enabled())
}

func TestEngine_RebuildView(t *testing.T) {
	db := setupTestDB(t)
	engine := New(db, Config{Enabled: true})
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"vw-1", "test_view", 5, 1, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	err = engine.RebuildView(ctx, "test_view")
	require.NoError(t, err)

	var watermark int64
	err = db.QueryRow("SELECT watermark FROM memory_materialized_views WHERE view_name = ?", "test_view").Scan(&watermark)
	require.NoError(t, err)
	require.Equal(t, int64(0), watermark)
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

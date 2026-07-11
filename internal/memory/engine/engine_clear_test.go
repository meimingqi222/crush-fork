package engine

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestEngineClear verifies that Clear wipes all memory-owned tables (not
// just memory_events) and resets in-memory pipeline state, so Status()
// reflects a clean slate immediately -- backing the "Memory: Clear" user
// command (docs/refactor-memory.md Phase 4).
func TestEngineClear(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	eng := New(conn, Config{Enabled: true, Backend: "local"})

	evt := MemoryEvent{
		ID:        "evt-1",
		Scope:     MemoryScopeProject,
		Kind:      MemoryKindDecision,
		Content:   "Use SQLite for persistence",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, eng.EventStore().Append(t.Context(), evt))

	events, err := eng.EventStore().Query(t.Context(), EventFilter{Limit: 0})
	require.NoError(t, err)
	require.Len(t, events, 1)

	require.NoError(t, eng.tripleStore.AddTriple(t.Context(), Triple{
		Subject: "project", Predicate: "uses", Object: "SQLite",
		SourceEventID: evt.ID, ValidFrom: time.Now(),
	}))
	triples, err := eng.tripleStore.QueryTriples(t.Context(), "", "", 10)
	require.NoError(t, err)
	require.Len(t, triples, 1)

	require.NoError(t, eng.Clear(t.Context()))

	events, err = eng.EventStore().Query(t.Context(), EventFilter{Limit: 0})
	require.NoError(t, err)
	require.Empty(t, events)

	triples, err = eng.tripleStore.QueryTriples(t.Context(), "", "", 10)
	require.NoError(t, err)
	require.Empty(t, triples)

	status, err := eng.Status(t.Context())
	require.NoError(t, err)
	require.Nil(t, status.ExtractionStatus.LastRunAt)
	require.Nil(t, status.ConsolidationStatus.LastRunAt)
	require.Zero(t, status.ConsolidationStatus.LastWatermark)
}

// TestEngineClearNilDB verifies Clear is a no-op (not a panic) when the
// engine has no database attached.
func TestEngineClearNilDB(t *testing.T) {
	t.Parallel()

	eng := &Engine{}
	require.NoError(t, eng.Clear(t.Context()))
}

package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummaryRetrieverRetrieveRanksByQuery(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	sqlite := testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for local durable memory storage.")
	sqlite.Importance = 0.4
	unrelated := testEvent(MemoryScopeProject, MemoryKindPitfall, "Avoid stale generated schemas.")
	unrelated.Importance = 0.95
	userPreference := testEvent(MemoryScopeUser, MemoryKindPreference, "Prefer concise memory recall output.")
	userPreference.Importance = 0.8

	require.NoError(t, store.Append(ctx, unrelated))
	require.NoError(t, store.Append(ctx, userPreference))
	require.NoError(t, store.Append(ctx, sqlite))

	retriever := NewSummaryRetriever(store, "")
	events, err := retriever.Retrieve(ctx, "sqlite memory", map[string]any{"limit": 2})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, sqlite.ID, events[0].ID)
	require.Equal(t, userPreference.ID, events[1].ID)
}

func TestSummaryRetrieverRetrieveEmptyQueryUsesStoreOrder(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	first := testEvent(MemoryScopeProject, MemoryKindDecision, "First memory event.")
	second := testEvent(MemoryScopeProject, MemoryKindPitfall, "Second memory event.")
	require.NoError(t, store.Append(ctx, first))
	require.NoError(t, store.Append(ctx, second))

	retriever := NewSummaryRetriever(store, "")
	events, err := retriever.Retrieve(ctx, "", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, first.ID, events[0].ID)
}

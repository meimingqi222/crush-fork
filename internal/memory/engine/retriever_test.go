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
	sqlite.Importance = 1.0
	unrelated := testEvent(MemoryScopeProject, MemoryKindPitfall, "Avoid stale generated schemas.")
	unrelated.Importance = 0.95
	userPreference := testEvent(MemoryScopeUser, MemoryKindPreference, "Prefer concise memory recall output.")
	userPreference.Summary = "SQLite recall preference"
	userPreference.Importance = 0.1

	require.NoError(t, store.Append(ctx, unrelated))
	require.NoError(t, store.Append(ctx, userPreference))
	require.NoError(t, store.Append(ctx, sqlite))

	retriever := NewSummaryRetriever(store, db, "").WithReranker(NewHeuristicReranker())
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

	retriever := NewSummaryRetriever(store, db, "")
	events, err := retriever.Retrieve(ctx, "", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, first.ID, events[0].ID)
}

func TestSummaryRetrieverRetrieveBilingualQueryExpansion(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	compaction := testEvent(MemoryScopeProject, MemoryKindProcedure, "Before compaction, inject relevant memory recall into the summary prompt.")
	compaction.Importance = 0.6
	unrelated := testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for local durable memory storage.")
	unrelated.Importance = 0.9

	require.NoError(t, store.Append(ctx, unrelated))
	require.NoError(t, store.Append(ctx, compaction))

	retriever := NewSummaryRetriever(store, db, "").WithReranker(NewHeuristicReranker())
	events, err := retriever.Retrieve(ctx, "压缩前召回", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, compaction.ID, events[0].ID)
}

func TestQueryTermsHandlesMixedChineseEnglish(t *testing.T) {
	terms := queryTerms("记忆 recall 准确率")
	require.Contains(t, terms, "记")
	require.Contains(t, terms, "记忆")
	require.Contains(t, terms, "recall")
	require.Contains(t, terms, "memory")
}

func TestEmbeddingRerankerRanksMixedChineseEnglishCandidates(t *testing.T) {
	relevant := testEvent(MemoryScopeProject, MemoryKindProcedure, "Before compaction, retrieve relevant memories and inject recall into the summary prompt.")
	relevant.Watermark = 1
	unrelated := testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for local durable event storage.")
	unrelated.Watermark = 2

	reranker := NewEmbeddingReranker(NewHashingEmbedder(128))
	events, err := reranker.Rerank(context.Background(), "压缩前 recall", []MemoryEvent{unrelated, relevant})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, relevant.ID, events[0].ID)
}

func TestSummaryRetrieverRetrieveWithEmbeddingReranker(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	relevant := testEvent(MemoryScopeProject, MemoryKindProcedure, "Before compaction, retrieve relevant memories and inject recall into the summary prompt.")
	unrelated := testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for local durable event storage.")

	require.NoError(t, store.Append(ctx, unrelated))
	require.NoError(t, store.Append(ctx, relevant))

	retriever := NewSummaryRetriever(store, db, "").WithReranker(NewEmbeddingReranker(NewHashingEmbedder(128)))
	events, err := retriever.Retrieve(ctx, "压缩前 recall", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, relevant.ID, events[0].ID)
}

// countingEmbedder wraps an Embedder and counts Embed calls, used to verify
// that the "rerank" opt actually gates whether the embedding-based vector
// voice runs, rather than just changing the final ordering (which could be
// coincidental).
type countingEmbedder struct {
	Embedder
	calls int
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	c.calls++
	return c.Embedder.Embed(ctx, text)
}

// TestSummaryRetrieverRetrieveRerankOptGatesEmbeddingVoice is a regression
// test for docs/refactor-memory.md Phase 5 (P5.5): the per-turn auto-recall
// prefetch path (coordinator.buildAutoRecallBlock) passes "rerank": false to
// skip the embedding-based vector voice and the final heuristic rerank pass,
// while the explicit `recall` tool call leaves the option unset (defaulting
// to enabled) to preserve full-quality ranking. This asserts the embedder is
// not invoked at all when rerank is explicitly disabled, and is invoked when
// left at its default.
func TestSummaryRetrieverRetrieveRerankOptGatesEmbeddingVoice(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLiteEventStore(db)
	ctx := context.Background()

	relevant := testEvent(MemoryScopeProject, MemoryKindProcedure, "Before compaction, retrieve relevant memories and inject recall into the summary prompt.")
	unrelated := testEvent(MemoryScopeProject, MemoryKindDecision, "Use SQLite for local durable event storage.")
	require.NoError(t, store.Append(ctx, unrelated))
	require.NoError(t, store.Append(ctx, relevant))

	spy := &countingEmbedder{Embedder: NewHashingEmbedder(128)}
	retriever := NewSummaryRetriever(store, db, "").WithReranker(NewEmbeddingReranker(spy))

	// Explicit recall path (rerank left unset, defaults to enabled): the
	// embedder must be invoked.
	_, err := retriever.Retrieve(ctx, "压缩前 recall", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.Positive(t, spy.calls, "reranker must run when the rerank opt is left at its default")

	// Auto-recall prefetch path (rerank explicitly disabled): the embedder
	// must not be invoked at all.
	spy.calls = 0
	_, err = retriever.Retrieve(ctx, "压缩前 recall", map[string]any{"limit": 1, "rerank": false})
	require.NoError(t, err)
	require.Zero(t, spy.calls, "reranker must not run when rerank:false is set (auto-recall path)")
}

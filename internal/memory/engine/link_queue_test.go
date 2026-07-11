package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEngine_AfterTurnIdleLinksSynchronouslyWithoutGoroutine is a regression
// test for docs/refactor-memory.md Phase 5 (P5.3): AfterTurnIdle used to
// spawn `go e.proactiveLinker.LinkEvents(...)` as a fire-and-forget goroutine,
// so a test asserting on resulting edges would have been racy without a
// sleep. Proactive linking is now queued (enqueuePendingLinks) and drained
// serially inside the existing materialization pass
// (TriggerMaterialization/drainPendingLinks), so the edge must already exist
// by the time AfterTurnIdle returns -- no goroutine, no sleep required.
func TestEngine_AfterTurnIdleLinksSynchronouslyWithoutGoroutine(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	// Seed an existing durable memory that the newly extracted event should
	// link to (same scope/kind, lexically similar content).
	seed := testEvent(MemoryScopeProject, MemoryKindDecision, "use postgres for the database layer")
	require.NoError(t, eng.store.Append(ctx, seed))

	extractor := newMockExtractor(mockExtractorDeps{
		transcriptFn: defaultTranscriptFn(t, "USER: use postgres\nASSISTANT: ok", []string{"msg-link-1"}),
		analyzeFn: defaultAnalyzeFn(t, []ExtractedEvent{
			{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "use postgres for the database layer now", Confidence: 0.9, Importance: 0.8},
		}),
		filesFn: defaultFilesFn(nil),
		clock:   fixedClock,
	})
	eng.SetExtractor(extractor)

	require.NoError(t, eng.AfterTurnIdle(ctx, "sess-link-test", nil))

	var edgeCount int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_edges").Scan(&edgeCount)
	require.NoError(t, err)
	require.Greater(t, edgeCount, 0, "proactive linking must have run synchronously within AfterTurnIdle's materialization pass, with no edges requiring a sleep to observe")
}

// TestEngine_DrainPendingLinksIsIdempotentWhenEmpty verifies that draining an
// empty pending-link queue (the common case for most materialization passes)
// is a cheap no-op rather than an error.
func TestEngine_DrainPendingLinksIsIdempotentWhenEmpty(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})

	require.NoError(t, eng.TriggerMaterialization(context.Background()))
	require.NoError(t, eng.TriggerMaterialization(context.Background()))
}

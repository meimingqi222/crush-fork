package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestResolveConflict_SupersedesDirection verifies that resolving a conflict
// sets the supersedes field on the WINNER pointing at the LOSER — not the
// reverse. This matches the system-wide convention enforced by
// FilterLatestNonSuperseded and the consolidator: the event that does the
// superseding holds the ID of the event it retires.
//
// A previous version of ResolveConflict set supersedes=winningID on the losing
// event, which inverted the relationship and caused FilterLatestNonSuperseded
// to return the losing event instead of the winner.
func TestResolveConflict_SupersedesDirection(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	detector := NewConflictDetector(db)
	ctx := context.Background()

	store := NewSQLiteEventStore(db)

	// Two conflicting events: same scope+kind, different content.
	winner := testEvent(MemoryScopeProject, MemoryKindPreference, "prefer dark mode")
	winner.ID = "evt-winner"
	loser := testEvent(MemoryScopeProject, MemoryKindPreference, "prefer light mode")
	loser.ID = "evt-loser"
	// Append loser first so it has a lower watermark.
	require.NoError(t, store.Append(ctx, loser))
	require.NoError(t, store.Append(ctx, winner))

	// Detect the conflict between them.
	n, err := detector.DetectConflicts()
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "should detect at least one conflict")

	conflicts, err := detector.GetUnresolvedConflicts(10)
	require.NoError(t, err)
	require.NotEmpty(t, conflicts)

	var conflictID int64
	for _, c := range conflicts {
		if (c.FactAID == winner.ID && c.FactBID == loser.ID) ||
			(c.FactAID == loser.ID && c.FactBID == winner.ID) {
			conflictID = c.ID
			break
		}
	}
	require.NotZero(t, conflictID, "conflict between winner and loser should exist")

	// Resolve in favor of the winner.
	require.NoError(t, detector.ResolveConflict(conflictID, winner.ID))

	// Re-read both events to inspect the supersedes field.
	gotWinner, err := store.GetByID(ctx, winner.ID)
	require.NoError(t, err)
	gotLoser, err := store.GetByID(ctx, loser.ID)
	require.NoError(t, err)

	// The WINNER's supersedes must point at the LOSER's ID.
	require.NotNil(t, gotWinner.Supersedes, "winner should carry a non-nil supersedes")
	require.Equal(t, loser.ID, *gotWinner.Supersedes,
		"winner.supersedes must point at the losing event ID")

	// The LOSER's own supersedes must be nil — it is being retired, not doing
	// the superseding.
	require.Nil(t, gotLoser.Supersedes,
		"loser.supersedes must be nil; only the winner marks the relationship")
}

// TestResolveConflict_FilterReturnsWinner verifies the end-to-end effect of
// the correct supersedes direction: FilterLatestNonSuperseded returns the
// WINNER after resolution, not the loser.
func TestResolveConflict_FilterReturnsWinner(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	detector := NewConflictDetector(db)
	ctx := context.Background()

	store := NewSQLiteEventStore(db)

	winner := testEvent(MemoryScopeSession, MemoryKindWorkingMemory, "current state A")
	winner.ID = "evt-wm-winner"
	loser := testEvent(MemoryScopeSession, MemoryKindWorkingMemory, "current state B")
	loser.ID = "evt-wm-loser"
	// Note: working_memory is excluded from DetectConflicts, so insert the
	// conflict row manually to test the resolution path in isolation.
	require.NoError(t, store.Append(ctx, loser))
	require.NoError(t, store.Append(ctx, winner))

	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO memory_conflicts (fact_a_id, fact_b_id, conflict_type, created_at, updated_at)
		VALUES (?, ?, 'contradiction', ?, ?)
	`, loser.ID, winner.ID, now, now)
	require.NoError(t, err)

	conflicts, err := detector.GetUnresolvedConflicts(10)
	require.NoError(t, err)
	require.NotEmpty(t, conflicts)

	require.NoError(t, detector.ResolveConflict(conflicts[0].ID, winner.ID))

	// Query both events (ordered by watermark ascending, as
	// FilterLatestNonSuperseded expects) and verify the winner survives.
	events, err := store.Query(ctx, EventFilter{Limit: 10})
	require.NoError(t, err)
	latest := FilterLatestNonSuperseded(events)
	require.NotNil(t, latest, "should return a non-superseded event")
	require.Equal(t, winner.ID, latest.ID,
		"FilterLatestNonSuperseded must return the winner, not the loser")
}

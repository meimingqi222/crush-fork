package sessionevent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJournalEvictsByCount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	journal := NewJournal(3, time.Hour)
	for sequence := uint64(1); sequence <= 5; sequence++ {
		journal.Append(Event{Sequence: sequence, FirstSequence: sequence, Timestamp: now}, now)
	}

	events, available := journal.ReplayAfter(2, 5, now)
	require.True(t, available)
	require.Equal(t, []uint64{3, 4, 5}, eventSequences(events))
	_, available = journal.ReplayAfter(1, 5, now)
	require.False(t, available)
}

func TestJournalEvictsByAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	journal := NewJournal(10, time.Minute)
	journal.Append(Event{Sequence: 1, FirstSequence: 1, Timestamp: now.Add(-2 * time.Minute)}, now.Add(-2*time.Minute))
	journal.Append(Event{Sequence: 2, FirstSequence: 2, Timestamp: now}, now)

	_, available := journal.ReplayAfter(0, 2, now)
	require.False(t, available)
	events, available := journal.ReplayAfter(1, 2, now)
	require.True(t, available)
	require.Equal(t, []uint64{2}, eventSequences(events))
}

func TestJournalRejectsNonIncreasingSequence(t *testing.T) {
	t.Parallel()

	journal := NewJournal(2, 0)
	now := time.Now()
	journal.Append(Event{Sequence: 1, Timestamp: now}, now)
	require.Panics(t, func() {
		journal.Append(Event{Sequence: 1, Timestamp: now}, now)
	})
}

func eventSequences(events []Event) []uint64 {
	sequences := make([]uint64, len(events))
	for index, event := range events {
		sequences[index] = event.Sequence
	}
	return sequences
}

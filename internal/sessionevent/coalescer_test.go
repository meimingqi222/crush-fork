package sessionevent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultCoalescerMergesTextRange(t *testing.T) {
	t.Parallel()

	merged, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 4, Sequence: 5, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m:p", Payload: TextDelta{MessageID: "m", PartID: "p", Text: "hello "}},
		Event{SessionID: "s", FirstSequence: 6, Sequence: 6, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m:p", Payload: TextDelta{MessageID: "m", PartID: "p", Text: "world"}},
	)
	require.True(t, ok)
	require.Equal(t, uint64(4), merged.FirstSequence)
	require.Equal(t, uint64(6), merged.Sequence)
	require.Equal(t, "hello world", merged.Payload.(TextDelta).Text)
}

func TestDefaultCoalescerMergesContiguousTerminalOutput(t *testing.T) {
	t.Parallel()

	merged, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 1, Sequence: 1, Kind: KindTerminalOutput, Delivery: DeliveryMerge, CoalesceKey: "t", Payload: TerminalOutput{TerminalID: "t", Offset: 10, Data: []byte("abc")}},
		Event{SessionID: "s", FirstSequence: 2, Sequence: 2, Kind: KindTerminalOutput, Delivery: DeliveryMerge, CoalesceKey: "t", Payload: TerminalOutput{TerminalID: "t", Offset: 13, Data: []byte("def")}},
	)
	require.True(t, ok)
	require.Equal(t, uint64(10), merged.Payload.(TerminalOutput).Offset)
	require.Equal(t, []byte("abcdef"), merged.Payload.(TerminalOutput).Data)
}

func TestDefaultCoalescerRejectsSequenceGap(t *testing.T) {
	t.Parallel()

	_, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 1, Sequence: 1, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{}},
		Event{SessionID: "s", FirstSequence: 3, Sequence: 3, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{}},
	)
	require.False(t, ok)
}

func TestDefaultCoalescerHonorsMergeWindow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	_, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 1, Sequence: 1, Timestamp: now, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{}},
		Event{SessionID: "s", FirstSequence: 2, Sequence: 2, Timestamp: now.Add(34 * time.Millisecond), Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{}},
	)
	require.False(t, ok)
}

func TestDefaultCoalescerKeepsLatestEntityState(t *testing.T) {
	t.Parallel()

	merged, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 7, Sequence: 7, Kind: KindToolProgress, Delivery: DeliveryLatest, CoalesceKey: "tool", Payload: "25%"},
		Event{SessionID: "s", FirstSequence: 8, Sequence: 8, Kind: KindToolProgress, Delivery: DeliveryLatest, CoalesceKey: "tool", Payload: "50%"},
	)
	require.True(t, ok)
	require.Equal(t, uint64(7), merged.FirstSequence)
	require.Equal(t, uint64(8), merged.Sequence)
	require.Equal(t, "50%", merged.Payload)
}

func TestDefaultCoalescerBoundsMergedPayloadBytes(t *testing.T) {
	t.Parallel()

	_, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 1, Sequence: 1, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{Text: string(make([]byte, maxMergedPayloadBytes))}},
		Event{SessionID: "s", FirstSequence: 2, Sequence: 2, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", Payload: TextDelta{Text: "x"}},
	)
	require.False(t, ok)
}

func TestDefaultCoalescerBoundsMergedEventCount(t *testing.T) {
	t.Parallel()

	_, ok := (DefaultCoalescer{}).Merge(
		Event{SessionID: "s", FirstSequence: 1, Sequence: 31, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", MergedCount: 31, Payload: TextDelta{Text: "a"}},
		Event{SessionID: "s", FirstSequence: 32, Sequence: 33, Kind: KindMessageDelta, Delivery: DeliveryMerge, CoalesceKey: "m", MergedCount: 2, Payload: TextDelta{Text: "b"}},
	)
	require.False(t, ok)
}

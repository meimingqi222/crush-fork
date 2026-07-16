package sessionevent

import "time"

const (
	defaultMergeWindow    = 33 * time.Millisecond
	maxMergedPayloadBytes = 64 * 1024
	maxMergedEvents       = 32
)

// Coalescer combines subscriber-specific events without changing the journal.
type Coalescer interface {
	Merge(previous, next Event) (Event, bool)
}

// DefaultCoalescer implements the protocol backpressure table for text,
// reasoning, terminal output, and latest-wins entity updates.
type DefaultCoalescer struct{}

// Merge returns a single event covering both sequence ranges when compatible.
func (DefaultCoalescer) Merge(previous, next Event) (Event, bool) {
	if previous.SessionID != next.SessionID || previous.Delivery != next.Delivery ||
		previous.Kind != next.Kind || previous.CoalesceKey == "" ||
		previous.CoalesceKey != next.CoalesceKey || previous.Sequence+1 != next.FirstSequence ||
		next.Timestamp.Sub(previous.Timestamp) > defaultMergeWindow {
		return Event{}, false
	}

	switch next.Delivery {
	case DeliveryLatest:
		next.FirstSequence = previous.FirstSequence
		return next, true
	case DeliveryMerge:
		previousCount := max(previous.MergedCount, 1)
		nextCount := max(next.MergedCount, 1)
		if int(previousCount)+int(nextCount) > maxMergedEvents {
			return Event{}, false
		}
		next.MergedCount = previousCount + nextCount
		next.Timestamp = previous.Timestamp
		return mergePayload(previous, next)
	default:
		return Event{}, false
	}
}

func mergePayload(previous, next Event) (Event, bool) {
	switch previousPayload := previous.Payload.(type) {
	case TextDelta:
		nextPayload, ok := next.Payload.(TextDelta)
		if !ok || previousPayload.MessageID != nextPayload.MessageID || previousPayload.PartID != nextPayload.PartID {
			return Event{}, false
		}
		if len(previousPayload.Text)+len(nextPayload.Text) > maxMergedPayloadBytes {
			return Event{}, false
		}
		nextPayload.Text = previousPayload.Text + nextPayload.Text
		next.Payload = nextPayload
	case TerminalOutput:
		nextPayload, ok := next.Payload.(TerminalOutput)
		if !ok || previousPayload.TerminalID != nextPayload.TerminalID ||
			previousPayload.Offset+uint64(len(previousPayload.Data)) != nextPayload.Offset {
			return Event{}, false
		}
		if len(previousPayload.Data)+len(nextPayload.Data) > maxMergedPayloadBytes {
			return Event{}, false
		}
		data := make([]byte, 0, len(previousPayload.Data)+len(nextPayload.Data))
		data = append(data, previousPayload.Data...)
		data = append(data, nextPayload.Data...)
		nextPayload.Offset = previousPayload.Offset
		nextPayload.Data = data
		next.Payload = nextPayload
	default:
		return Event{}, false
	}
	next.FirstSequence = previous.FirstSequence
	return next, true
}

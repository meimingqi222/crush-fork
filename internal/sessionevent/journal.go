package sessionevent

import "time"

// Journal is a bounded, non-thread-safe event ring. Its owner provides
// synchronization so sequence allocation and append remain atomic.
type Journal struct {
	maxEvents    int
	maxAge       time.Duration
	buffer       []Event
	start        int
	size         int
	lastSequence uint64
	hasLast      bool
}

// NewJournal creates a journal bounded by count and age. A non-positive age
// disables age eviction; maxEvents must be positive.
func NewJournal(maxEvents int, maxAge time.Duration) *Journal {
	if maxEvents <= 0 {
		panic("sessionevent: journal maxEvents must be positive")
	}
	return &Journal{
		maxEvents: maxEvents,
		maxAge:    maxAge,
		buffer:    make([]Event, maxEvents),
	}
}

// Append adds an event and evicts expired entries. Events must be appended in
// strictly increasing sequence order.
func (j *Journal) Append(event Event, now time.Time) {
	if j.hasLast && event.Sequence <= j.lastSequence {
		panic("sessionevent: journal sequence must increase")
	}
	j.lastSequence = event.Sequence
	j.hasLast = true
	if j.size < j.maxEvents {
		index := (j.start + j.size) % j.maxEvents
		j.buffer[index] = event
		j.size++
	} else {
		j.buffer[j.start] = event
		j.start = (j.start + 1) % j.maxEvents
	}
	j.prune(now)
}

// ReplayAfter returns events after sequence. available is false when the
// requested sequence predates the retained range.
func (j *Journal) ReplayAfter(sequence, latest uint64, now time.Time) ([]Event, bool) {
	j.prune(now)
	if sequence >= latest {
		return []Event{}, true
	}
	if j.size == 0 || sequence+1 < j.at(0).Sequence {
		return nil, false
	}
	index := 0
	for index < j.size && j.at(index).Sequence <= sequence {
		index++
	}
	result := make([]Event, j.size-index)
	for resultIndex := range result {
		result[resultIndex] = j.at(index + resultIndex)
	}
	return result, true
}

func (j *Journal) prune(now time.Time) {
	if j.maxAge <= 0 || j.size == 0 {
		return
	}
	cutoff := now.Add(-j.maxAge)
	for j.size > 0 && j.at(0).Timestamp.Before(cutoff) {
		j.buffer[j.start] = Event{}
		j.start = (j.start + 1) % j.maxEvents
		j.size--
	}
}

func (j *Journal) at(index int) Event {
	return j.buffer[(j.start+index)%j.maxEvents]
}

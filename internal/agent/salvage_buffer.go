package agent

import (
	"strings"
	"sync"
)

// salvageMaxRunes caps how much trailing assistant text the salvage buffer
// retains. 4 KiB is enough to capture the final paragraph / tool-call summary
// without materially growing memory usage per run.
const salvageMaxRunes = 4 * 1024

// salvageBuffer is a bounded ring buffer that retains the trailing slice of
// streamed assistant text. It exists so that when an agent run is canceled
// mid-stream, the partial output produced just before cancellation can be
// surfaced to the caller (e.g. attached to the synthetic tool result returned
// to the parent agent) instead of being discarded.
//
// The buffer is safe for concurrent use because OnTextDelta callbacks may
// fire from the fantasy stream goroutine while the error-finalization path
// reads from the Run goroutine.
type salvageBuffer struct {
	mu   sync.Mutex
	data []rune
	max  int
}

func newSalvageBuffer(maxRunes int) *salvageBuffer {
	if maxRunes <= 0 {
		maxRunes = salvageMaxRunes
	}
	return &salvageBuffer{max: maxRunes}
}

// append records s into the buffer, evicting the oldest runes when the cap is
// exceeded. Trailing whitespace is trimmed on read, not on write, so callers
// can stream fragments without per-call overhead.
func (b *salvageBuffer) append(s string) {
	if b == nil || s == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, []rune(s)...)
	if len(b.data) > b.max*2 {
		// Keep only the most recent max runes to bound memory between evictions.
		b.data = b.data[len(b.data)-b.max:]
	}
}

// snapshot returns a trimmed copy of the buffered text. Returns "" when empty.
func (b *salvageBuffer) snapshot() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) == 0 {
		return ""
	}
	// Bound to max in case append pushed past it without an eviction yet.
	start := 0
	if len(b.data) > b.max {
		start = len(b.data) - b.max
	}
	return strings.TrimSpace(string(b.data[start:]))
}

// reset clears the buffer. Called at the start of each stream attempt so a
// retry does not surface stale text from a prior failed attempt.
func (b *salvageBuffer) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
}

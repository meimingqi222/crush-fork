package agent

import (
	"fmt"
	"sync"
)

// subagentIDAllocator deduplicates subagent task names within a single
// runSubagents invocation. The first occurrence of a name keeps the original
// value; subsequent occurrences get a numeric suffix (-2, -3, ...). This
// prevents AgentRegistry key collisions and IRC peer ID clashes when the
// parent LLM fans out multiple tasks that happen to share a name.
//
// The allocator is not persisted across runs: each runSubagents call creates a
// fresh allocator. Cross-run deduplication is handled by the
// SubagentLifecycleManager, which keeps completed child sessions adoptable for
// a TTL window so a follow-up agent tool call with ExistingSessionID can reuse
// the live SessionAgent instance instead of rebuilding one from disk.
type subagentIDAllocator struct {
	mu   sync.Mutex
	seen map[string]int
}

func newSubagentIDAllocator() *subagentIDAllocator {
	return &subagentIDAllocator{seen: make(map[string]int)}
}

// Alloc returns a deduplicated name for the given base. The first call for a
// base returns base unchanged; subsequent calls return "<base>-<n>" where n
// starts at 2. An empty base is treated as "task".
func (a *subagentIDAllocator) Alloc(base string) string {
	base = sanitizeSubagentNameBase(base)
	a.mu.Lock()
	defer a.mu.Unlock()
	count := a.seen[base]
	a.seen[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, count+1)
}

// sanitizeSubagentNameBase strips surrounding whitespace and replaces any
// run of characters that are unsafe for registry keys / IRC peer IDs with a
// single hyphen. The result is guaranteed non-empty.
func sanitizeSubagentNameBase(name string) string {
	name = sanitizeForID(name)
	if name == "" {
		return "task"
	}
	return name
}

// sanitizeForID keeps alphanumerics, underscores and hyphens; everything else
// collapses to a hyphen. Leading/trailing hyphens are trimmed.
func sanitizeForID(name string) string {
	out := make([]rune, 0, len(name))
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			out = append(out, r)
			prevDash = false
		default:
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	// Trim trailing dash.
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

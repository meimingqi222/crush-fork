package autopermission

import (
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/google/uuid"
)

const defaultMaxDenialQueueSize = 10

// DenialEntry represents a permission request that was blocked by the Guardian classifier.
type DenialEntry struct {
	ID        string
	Request   permission.PermissionRequest
	Reason    string
	Timestamp time.Time
	Retryable bool
}

// DenialQueue maintains a bounded queue of recent Guardian denials.
// Users can approve denied actions to retry them.
type DenialQueue struct {
	mu      sync.RWMutex
	entries []*DenialEntry
	maxSize int
}

// NewDenialQueue creates a new denial queue with the specified max size.
func NewDenialQueue(maxSize int) *DenialQueue {
	if maxSize <= 0 {
		maxSize = defaultMaxDenialQueueSize
	}
	return &DenialQueue{
		entries: make([]*DenialEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

// Push adds a new denial entry to the queue.
// If the queue is full, the oldest entry is removed.
// If an entry with the same request already exists, it is replaced.
func (q *DenialQueue) Push(req permission.PermissionRequest, reason string, retryable bool) *DenialEntry {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry := &DenialEntry{
		ID:        uuid.New().String(),
		Request:   req,
		Reason:    reason,
		Timestamp: time.Now(),
		Retryable: retryable,
	}

	// Remove existing entry with same tool call ID if present
	for i, e := range q.entries {
		if e.Request.ToolCallID == req.ToolCallID {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			break
		}
	}

	// Add to front (most recent first)
	q.entries = append([]*DenialEntry{entry}, q.entries...)

	// Trim to max size
	if len(q.entries) > q.maxSize {
		q.entries = q.entries[:q.maxSize]
	}

	return entry
}

// Take removes and returns the entry with the given ID.
// Returns nil if not found.
func (q *DenialQueue) Take(id string) *DenialEntry {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, e := range q.entries {
		if e.ID == id {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			return e
		}
	}
	return nil
}

// Get returns the entry with the given ID without removing it.
// Returns nil if not found.
func (q *DenialQueue) Get(id string) *DenialEntry {
	q.mu.RLock()
	defer q.mu.RUnlock()

	for _, e := range q.entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// Entries returns a copy of all entries in the queue.
func (q *DenialQueue) Entries() []*DenialEntry {
	q.mu.RLock()
	defer q.mu.RUnlock()

	result := make([]*DenialEntry, len(q.entries))
	copy(result, q.entries)
	return result
}

// Size returns the current number of entries in the queue.
func (q *DenialQueue) Size() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.entries)
}

// IsEmpty returns true if the queue has no entries.
func (q *DenialQueue) IsEmpty() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.entries) == 0
}

// Clear removes all entries from the queue.
func (q *DenialQueue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries = q.entries[:0]
}

// AsPermissionEntries converts the queue entries to permission.DenialQueueEntry format.
func (q *DenialQueue) AsPermissionEntries() []*permission.DenialQueueEntry {
	q.mu.RLock()
	defer q.mu.RUnlock()

	result := make([]*permission.DenialQueueEntry, len(q.entries))
	for i, e := range q.entries {
		result[i] = &permission.DenialQueueEntry{
			ID:        e.ID,
			Request:   e.Request,
			Reason:    e.Reason,
			Timestamp: e.Timestamp,
			Retryable: e.Retryable,
		}
	}
	return result
}

// ActionSummary returns a human-readable summary of the denied action.
func ActionSummary(entry *DenialEntry) string {
	if entry == nil {
		return "unknown action"
	}

	req := entry.Request
	switch req.ToolName {
	case "bash":
		if params, ok := req.Params.(map[string]any); ok {
			if cmd, ok := params["command"].(string); ok {
				return cmd
			}
		}
		return "bash command"
	case "edit":
		if params, ok := req.Params.(map[string]any); ok {
			if path, ok := params["file_path"].(string); ok {
				return "edit " + path
			}
		}
		return "file edit"
	case "write":
		if params, ok := req.Params.(map[string]any); ok {
			if path, ok := params["file_path"].(string); ok {
				return "write " + path
			}
		}
		return "file write"
	default:
		return req.ToolName + " " + req.Action
	}
}

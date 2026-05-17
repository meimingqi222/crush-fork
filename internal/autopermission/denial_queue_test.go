package autopermission

import (
	"testing"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestDenialQueue_Push(t *testing.T) {
	q := NewDenialQueue(3)

	req1 := permission.PermissionRequest{ID: "1", ToolCallID: "tc1", ToolName: "bash"}
	req2 := permission.PermissionRequest{ID: "2", ToolCallID: "tc2", ToolName: "edit"}
	req3 := permission.PermissionRequest{ID: "3", ToolCallID: "tc3", ToolName: "write"}
	req4 := permission.PermissionRequest{ID: "4", ToolCallID: "tc4", ToolName: "bash"}

	q.Push(req1, "reason1", true)
	require.Equal(t, 1, q.Size())

	q.Push(req2, "reason2", true)
	require.Equal(t, 2, q.Size())

	q.Push(req3, "reason3", false)
	require.Equal(t, 3, q.Size())

	// Adding 4th should evict oldest
	q.Push(req4, "reason4", true)
	require.Equal(t, 3, q.Size())

	entries := q.Entries()
	require.Len(t, entries, 3)
	require.Equal(t, "4", entries[0].Request.ID) // most recent first
	require.Equal(t, "3", entries[1].Request.ID)
	require.Equal(t, "2", entries[2].Request.ID)
	require.Equal(t, "reason4", entries[0].Reason)
	require.True(t, entries[0].Retryable)
	require.False(t, entries[1].Retryable)
}

func TestDenialQueue_PushReplacesDuplicate(t *testing.T) {
	q := NewDenialQueue(10)

	req := permission.PermissionRequest{ID: "1", ToolCallID: "tc1", ToolName: "bash"}
	q.Push(req, "first reason", true)
	require.Equal(t, 1, q.Size())

	// Push same tool call ID again
	req2 := permission.PermissionRequest{ID: "2", ToolCallID: "tc1", ToolName: "bash"}
	q.Push(req2, "second reason", false)
	require.Equal(t, 1, q.Size())

	entries := q.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, "2", entries[0].Request.ID)
	require.Equal(t, "second reason", entries[0].Reason)
	require.False(t, entries[0].Retryable)
}

func TestDenialQueue_Take(t *testing.T) {
	q := NewDenialQueue(10)

	req := permission.PermissionRequest{ID: "1", ToolCallID: "tc1", ToolName: "bash"}
	entry := q.Push(req, "reason", true)

	require.Equal(t, 1, q.Size())

	// Take existing entry
	taken := q.Take(entry.ID)
	require.NotNil(t, taken)
	require.Equal(t, entry.ID, taken.ID)
	require.Equal(t, 0, q.Size())

	// Take non-existing
	taken = q.Take("nonexistent")
	require.Nil(t, taken)
}

func TestDenialQueue_Get(t *testing.T) {
	q := NewDenialQueue(10)

	req := permission.PermissionRequest{ID: "1", ToolCallID: "tc1", ToolName: "bash"}
	entry := q.Push(req, "reason", true)

	// Get existing
	got := q.Get(entry.ID)
	require.NotNil(t, got)
	require.Equal(t, entry.ID, got.ID)
	require.Equal(t, 1, q.Size()) // Not removed

	// Get non-existing
	got = q.Get("nonexistent")
	require.Nil(t, got)
}

func TestDenialQueue_Clear(t *testing.T) {
	q := NewDenialQueue(10)

	q.Push(permission.PermissionRequest{ID: "1", ToolCallID: "tc1"}, "r1", true)
	q.Push(permission.PermissionRequest{ID: "2", ToolCallID: "tc2"}, "r2", true)
	require.Equal(t, 2, q.Size())

	q.Clear()
	require.Equal(t, 0, q.Size())
	require.True(t, q.IsEmpty())
}

func TestDenialQueue_DefaultSize(t *testing.T) {
	q := NewDenialQueue(0) // Should use default
	require.Equal(t, 0, q.Size())

	// Fill to default max (10)
	for i := 0; i < 15; i++ {
		q.Push(permission.PermissionRequest{ID: string(rune('a' + i)), ToolCallID: string(rune('a' + i))}, "reason", true)
	}
	require.Equal(t, defaultMaxDenialQueueSize, q.Size())
}

func TestDenialQueue_ConcurrentAccess(t *testing.T) {
	q := NewDenialQueue(100)
	done := make(chan bool)

	// Concurrent writers
	for i := 0; i < 10; i++ {
		go func(n int) {
			for j := 0; j < 100; j++ {
				q.Push(permission.PermissionRequest{ID: string(rune(n*100 + j))}, "reason", true)
			}
			done <- true
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = q.Entries()
				_ = q.Size()
				_ = q.IsEmpty()
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 15; i++ {
		<-done
	}

	// Queue should not exceed max size
	require.LessOrEqual(t, q.Size(), 100)
}

func TestActionSummary(t *testing.T) {
	tests := []struct {
		name     string
		entry    *DenialEntry
		expected string
	}{
		{
			name:     "nil entry",
			entry:    nil,
			expected: "unknown action",
		},
		{
			name: "bash with command",
			entry: &DenialEntry{
				Request: permission.PermissionRequest{
					ToolName: "bash",
					Params:   map[string]any{"command": "ls -la"},
				},
			},
			expected: "ls -la",
		},
		{
			name: "bash without params",
			entry: &DenialEntry{
				Request: permission.PermissionRequest{ToolName: "bash"},
			},
			expected: "bash command",
		},
		{
			name: "edit with path",
			entry: &DenialEntry{
				Request: permission.PermissionRequest{
					ToolName: "edit",
					Params:   map[string]any{"file_path": "/tmp/test.go"},
				},
			},
			expected: "edit /tmp/test.go",
		},
		{
			name: "write with path",
			entry: &DenialEntry{
				Request: permission.PermissionRequest{
					ToolName: "write",
					Params:   map[string]any{"file_path": "/tmp/new.go"},
				},
			},
			expected: "write /tmp/new.go",
		},
		{
			name: "other tool",
			entry: &DenialEntry{
				Request: permission.PermissionRequest{
					ToolName: "grep",
					Action:   "search",
				},
			},
			expected: "grep search",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, ActionSummary(tt.entry))
		})
	}
}

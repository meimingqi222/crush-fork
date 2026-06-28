package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubagentIDAllocator_FirstKeepsOriginal(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	require.Equal(t, "explore", a.Alloc("explore"))
}

func TestSubagentIDAllocator_DuplicateGetsSuffix(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	require.Equal(t, "explore", a.Alloc("explore"))
	require.Equal(t, "explore-2", a.Alloc("explore"))
	require.Equal(t, "explore-3", a.Alloc("explore"))
}

func TestSubagentIDAllocator_DifferentNamesIndependent(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	require.Equal(t, "explore", a.Alloc("explore"))
	require.Equal(t, "plan", a.Alloc("plan"))
	require.Equal(t, "explore-2", a.Alloc("explore"))
	require.Equal(t, "plan-2", a.Alloc("plan"))
}

func TestSubagentIDAllocator_SanitizesUnsafeChars(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	require.Equal(t, "fix-bug", a.Alloc("fix bug"))
	require.Equal(t, "fix-bug-2", a.Alloc("fix bug"))
}

func TestSubagentIDAllocator_EmptyBecomesTask(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	require.Equal(t, "task", a.Alloc(""))
	require.Equal(t, "task-2", a.Alloc("   "))
}

func TestSubagentIDAllocator_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	a := newSubagentIDAllocator()
	done := make(chan string, 10)
	for range 10 {
		go func() { done <- a.Alloc("worker") }()
	}
	seen := make(map[string]int)
	for range 10 {
		seen[<-done]++
	}
	// All 10 allocations should produce unique names.
	require.Len(t, seen, 10)
}

package agent

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSalvageBuffer_BasicAppendAndSnapshot(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(salvageMaxRunes)
	b.append("Hello, ")
	b.append("world!")
	require.Equal(t, "Hello, world!", b.snapshot())
}

func TestSalvageBuffer_Empty(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(salvageMaxRunes)
	require.Equal(t, "", b.snapshot())

	b.append("")
	require.Equal(t, "", b.snapshot())
}

func TestSalvageBuffer_TrimsWhitespace(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(salvageMaxRunes)
	b.append("   \n\npartial reasoning...\n  \n")
	require.Equal(t, "partial reasoning...", b.snapshot())
}

func TestSalvageBuffer_EvictsOlderTextBeyondMax(t *testing.T) {
	t.Parallel()

	const max = 32
	b := newSalvageBuffer(max)

	// Write ~3x the cap so eviction kicks in.
	first := strings.Repeat("a", max)
	second := strings.Repeat("b", max)
	third := strings.Repeat("c", max)
	b.append(first)
	b.append(second)
	b.append(third)

	got := b.snapshot()
	require.LessOrEqual(t, len(got), max)
	require.True(t, strings.HasPrefix(got, "c"), "snapshot should retain most recent text; got prefix=%q", got[:1])
	require.NotContains(t, got, "a", "first (oldest) chunk should have been evicted")
}

func TestSalvageBuffer_Reset(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(salvageMaxRunes)
	b.append("leftover from previous attempt")
	b.reset()
	require.Equal(t, "", b.snapshot())

	b.append("fresh content")
	require.Equal(t, "fresh content", b.snapshot())
}

func TestSalvageBuffer_NilSafe(t *testing.T) {
	t.Parallel()

	var b *salvageBuffer
	require.NotPanics(t, func() {
		b.append("ignored")
		_ = b.snapshot()
		b.reset()
	})
	require.Equal(t, "", b.snapshot())
}

func TestSalvageBuffer_DefaultsMaxWhenNonPositive(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(0)
	require.Equal(t, salvageMaxRunes, b.max)

	b2 := newSalvageBuffer(-5)
	require.Equal(t, salvageMaxRunes, b2.max)
}

func TestSalvageBuffer_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := newSalvageBuffer(1024)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.append(strings.Repeat("x", i+1))
			_ = b.snapshot()
		}(i)
	}
	wg.Wait()
	// Final snapshot should be non-empty without panicking.
	_ = b.snapshot()
}

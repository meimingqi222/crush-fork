package list

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixedItem is a minimal Item whose rendered height is exactly lines,
// independent of width, so offset math can be tested deterministically.
type fixedItem struct{ lines int }

func (f *fixedItem) Render(width int) string {
	return strings.Repeat("x\n", f.lines-1) + "x"
}

// TestItemStartOffset verifies the item-top line math, including gap handling.
// This is the coordinate system that scroll-position adjustments after
// collapse/expand rely on, so regressions here produce visible viewport jumps.
func TestItemStartOffset(t *testing.T) {
	t.Parallel()

	l := NewList(&fixedItem{lines: 3}, &fixedItem{lines: 5}, &fixedItem{lines: 4})
	l.SetSize(120, 20)

	tests := []struct {
		name string
		gap  int
		idx  int
		want int
	}{
		{"first item starts at 0", 0, 0, 0},
		{"second item after first", 0, 1, 3},
		{"third item after first two", 0, 2, 8},
		{"past last item returns 0", 0, 3, 0},
		{"negative index returns 0", 0, -1, 0},
		{"gap counts between items", 1, 1, 4},
		{"gap accumulates", 1, 2, 10},
		{"gap past last item returns 0", 1, 3, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l.SetGap(tc.gap)
			require.Equal(t, tc.want, l.ItemStartOffset(tc.idx))
		})
	}
}

// TestItemStartOffsetAgreesWithViewportOrigin verifies ItemStartOffset and
// Offset share one coordinate system: scrolling so an item's top sits exactly
// at the viewport origin must make Offset() equal ItemStartOffset(idx).
func TestItemStartOffsetAgreesWithViewportOrigin(t *testing.T) {
	t.Parallel()

	for _, gap := range []int{0, 1} {
		l := NewList(&fixedItem{lines: 3}, &fixedItem{lines: 5}, &fixedItem{lines: 4})
		// Viewport smaller than the 12/15-line content so SetOffset's
		// maxOffset clamp cannot interfere.
		l.SetSize(120, 3)
		l.SetGap(gap)

		for idx := 1; idx < 3; idx++ {
			l.SetOffset(l.ItemStartOffset(idx))
			require.Equal(t, l.ItemStartOffset(idx), l.Offset(), "gap=%d idx=%d", gap, idx)
		}
	}
}

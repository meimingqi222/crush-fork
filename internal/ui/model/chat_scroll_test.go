package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// fixedHeightMsg is a minimal.MessageItem with a deterministic rendered
// height, so viewport scroll-position math can be tested without depending on
// real message rendering.
type fixedHeightItem struct {
	id     string
	height int
}

func (s *fixedHeightItem) ID() string { return s.id }

func (s *fixedHeightItem) Render(width int) string {
	return strings.Repeat("x\n", s.height-1) + "x"
}

func (s *fixedHeightItem) RawRender(width int) string { return s.Render(width) }

// newAdjustableChat builds a chat whose list has fixed-height items. NewChat
// assigns a 1px gap between items, so with item heights [2,3,40,2,30] the items
// start at content lines 0,3,7,48,51 and the viewport (height 5) allows line
// offsets up to 76.
func newAdjustableChat(t *testing.T) *Chat {
	t.Helper()
	theme := styles.DefaultStyles()
	c := NewChat(&common.Common{Styles: &theme})
	c.SetSize(120, 5)
	c.SetMessages(
		&fixedHeightItem{id: "a", height: 2},
		&fixedHeightItem{id: "b", height: 3},
		&fixedHeightItem{id: "c", height: 40}, // the collapsible item
		&fixedHeightItem{id: "d", height: 2},
		&fixedHeightItem{id: "e", height: 30},
	)
	// SetMessages anchors to the bottom; home the viewport before exercising
	// adjustOffsetForHeightChange.
	c.list.SetOffset(0)
	return c
}

// TestAdjustOffsetForHeightChange verifies the collapse gate in
// adjustOffsetForHeightChange: the line offset must only shrink when the
// collapsed item ends entirely above the viewport origin, and must stay put
// otherwise — including when the item itself holds the origin. This guards the
// regression that previously jumped the viewport up by the full collapse amount
// whenever a panel collapsed mid-view.
func TestAdjustOffsetForHeightChange(t *testing.T) {
	t.Parallel()

	t.Run("collapsing an item inside the viewport leaves offset unchanged", func(t *testing.T) {
		c := newAdjustableChat(t)
		c.list.SetOffset(20) // origin inside large item "c" (lines 7-48)
		c.adjustOffsetForHeightChange(2, 10)
		require.Equal(t, 20, c.list.Offset())
	})

	t.Run("collapsing the item at the viewport origin leaves offset unchanged", func(t *testing.T) {
		c := newAdjustableChat(t)
		c.list.SetOffset(3) // origin at the top of item "b"
		c.adjustOffsetForHeightChange(1, 1)
		require.Equal(t, 3, c.list.Offset())
	})

	t.Run("collapsing an item entirely above the origin reduces offset by the height", func(t *testing.T) {
		c := newAdjustableChat(t)
		c.list.SetOffset(65) // origin below item "c" (old bottom = 7+40+10 = 57)
		c.adjustOffsetForHeightChange(2, 10)
		require.Equal(t, 55, c.list.Offset())
	})

	t.Run("non-positive heightDiff is a no-op", func(t *testing.T) {
		c := newAdjustableChat(t)
		c.list.SetOffset(65)
		c.adjustOffsetForHeightChange(2, 0)
		c.adjustOffsetForHeightChange(2, -5)
		require.Equal(t, 65, c.list.Offset())
	})
}

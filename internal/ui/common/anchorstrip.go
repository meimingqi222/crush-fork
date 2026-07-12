package common

import (
	"strings"

	"github.com/charmbracelet/crush/internal/ui/styles"
)

// AnchorStrip renders a vertical strip of markers indicating the positions of
// user messages within the chat content. The marker for the user message that
// is currently closest to the top of the viewport is highlighted.
//
// Each marker is a single column character. The strip is height cells tall and
// is intended to be drawn immediately to the left of the scrollbar.
func AnchorStrip(s *styles.Styles, height, contentSize int, userOffsets []int, viewportOffset int) string {
	if height <= 0 || contentSize <= 0 || len(userOffsets) == 0 {
		return ""
	}

	// Highlight the user message whose top is nearest to but not below the
	// viewport offset. This mirrors scroll-to-anchor behavior: the marker just
	// above/at the current view is active.
	activeIdx := -1
	bestDist := int(^uint(0) >> 1)
	for i, off := range userOffsets {
		if off > viewportOffset+height {
			continue
		}
		dist := viewportOffset - off
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist {
			bestDist = dist
			activeIdx = i
		}
	}

	trackStyle := s.Dialog.ScrollbarTrack
	thumbStyle := s.Dialog.ScrollbarThumb

	var sb strings.Builder
	for i := range height {
		if i > 0 {
			sb.WriteByte('\n')
		}

		// Find whether a user message marker lands on this row.
		marked := false
		highlighted := false
		for idx, off := range userOffsets {
			row := off * height / max(1, contentSize)
			if row > height-1 {
				row = height - 1
			}
			if row == i {
				marked = true
				highlighted = idx == activeIdx
				break
			}
		}

		if marked {
			if highlighted {
				sb.WriteString(thumbStyle.Render(styles.ScrollbarThumb))
			} else {
				sb.WriteString(trackStyle.Render(styles.ScrollbarTrack))
			}
		} else {
			sb.WriteString(" ")
		}
	}

	return sb.String()
}

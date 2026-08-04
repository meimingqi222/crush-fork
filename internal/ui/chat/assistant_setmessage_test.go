package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestAssistantLoadingRenderHasNoBlankContentLine(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	msg := message.Message{
		ID:        "assistant-loading",
		Role:      message.Assistant,
		CreatedAt: time.Now().Unix(),
	}
	item := NewAssistantMessageItem(&sty, &msg).(*AssistantMessageItem)

	require.Equal(t, 1, item.TotalHeight(80))
	rendered := item.RenderVisible(80, 0, 1)
	plain := ansi.Strip(rendered)
	require.NotContains(t, plain, "\n\n")
	require.True(t, strings.Contains(plain, "Thinking ("), "loading spinner should show its elapsed time")
}

func TestAssistantSetMessageInvalidatesOnFinishTransition(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	msg := message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Hello"},
		},
	}
	item := NewAssistantMessageItem(&sty, &msg).(*AssistantMessageItem)

	_ = item.SetMessage(&msg)
	item.lastInvalidation = time.Now()
	item.setCachedRender("cached", 80, 1)

	streaming := message.Message{
		ID:   msg.ID,
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Hello"},
		},
	}
	_ = item.SetMessage(&streaming)
	_, _, ok := item.getCachedRender(80)
	require.True(t, ok, "streaming update within throttle window should keep cache")

	finished := message.Message{
		ID:   msg.ID,
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Hello"},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: 1},
		},
	}
	_ = item.SetMessage(&finished)
	_, _, ok = item.getCachedRender(80)
	require.False(t, ok, "finish transition must bypass streaming throttle")
}

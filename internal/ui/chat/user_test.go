package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestUserMessageItemHidesGuidedGoalProtocol(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	msg := &message.Message{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: `<guided_goal>
You are helping the user define an autonomous goal.

Rough goal from the user:
为啥这个光标问题经常发生？

Rules:
- Ask questions.
</guided_goal>`},
		},
	}
	item := NewUserMessageItem(&sty, msg, nil)

	rendered := ansi.Strip(item.RawRender(100))

	require.Contains(t, rendered, "Guided goal:")
	require.Contains(t, rendered, "为啥这个光标问题经常发生？")
	require.NotContains(t, rendered, "<guided_goal>")
	require.NotContains(t, rendered, "Rules:")
}

func TestAssistantMessageItemHidesKnownInternalBlocks(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	msg := &message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "<think>private reasoning</think>Visible answer."},
		},
	}
	item := NewAssistantMessageItem(&sty, msg)

	rendered := ansi.Strip(item.RawRender(100))

	require.Contains(t, rendered, "Visible answer.")
	require.NotContains(t, rendered, "private reasoning")
}

func TestToolMessageItemPreservesToolResultTags(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	item := NewGenericToolMessageItem(&sty, message.ToolCall{
		ID:       "tool-1",
		Name:     "custom_tool",
		Input:    `{}`,
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-1",
		Name:       "custom_tool",
		Content:    "<result>tool output</result>",
	}, false)

	rendered := ansi.Strip(item.RawRender(100))

	require.Contains(t, rendered, "<result>tool output</result>")
}

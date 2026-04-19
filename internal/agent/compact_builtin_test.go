package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func makeToolMsg(content string) message.Message {
	return message.Message{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call-1",
				Name:       "bash",
				Content:    content,
			},
		},
	}
}

func makeAssistantMsg() message.Message {
	return message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "I'll help."},
		},
	}
}

func makeUserMsg() message.Message {
	return message.Message{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Do something."},
		},
	}
}

func TestBuiltinPruneToolResults_NoPruneWhenSmall(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		makeUserMsg(),
		makeAssistantMsg(),
		makeToolMsg("small output"),
		makeAssistantMsg(),
		makeToolMsg("also small"),
		makeAssistantMsg(),
	}
	result := builtinPruneToolResults(msgs)
	require.Equal(t, len(msgs), len(result))
	// Content should be unchanged.
	tr := result[2].Parts[0].(message.ToolResult)
	require.Equal(t, "small output", tr.Content)
}

func TestBuiltinPruneToolResults_PrunesOldLargeToolResults(t *testing.T) {
	t.Parallel()
	bigContent := strings.Repeat("x", 200_000) // 200K chars

	// Build a conversation with many turns and large tool results.
	var msgs []message.Message
	for i := range 20 {
		msgs = append(msgs, makeUserMsg())
		msgs = append(msgs, makeAssistantMsg())
		msgs = append(msgs, makeToolMsg(fmt.Sprintf("large-output-%d: %s", i, bigContent)))
		msgs = append(msgs, makeAssistantMsg())
	}

	result := builtinPruneToolResults(msgs)
	require.Equal(t, len(msgs), len(result))

	// Recent turns should be preserved.
	lastToolIdx := len(result) - 2 // Last tool msg before last assistant.
	lastTR := result[lastToolIdx].Parts[0].(message.ToolResult)
	require.Contains(t, lastTR.Content, "large-output-19")

	// Old tool results should be pruned.
	firstTR := result[2].Parts[0].(message.ToolResult)
	require.Contains(t, firstTR.Content, "Old tool result content cleared")
	require.Contains(t, firstTR.Content, "characters omitted")
}

func TestBuiltinPruneToolResults_EmptyMessages(t *testing.T) {
	t.Parallel()
	result := builtinPruneToolResults(nil)
	require.Nil(t, result)

	result = builtinPruneToolResults([]message.Message{})
	require.Empty(t, result)
}

func TestBuiltinPruneToolResults_PreservesErrorToolResults(t *testing.T) {
	t.Parallel()
	bigContent := strings.Repeat("x", 200_000)

	var msgs []message.Message
	for range 20 {
		msgs = append(msgs, makeUserMsg())
		msgs = append(msgs, makeAssistantMsg())
		errorToolMsg := message.Message{
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: "call-err",
					Name:       "bash",
					Content:    bigContent,
					IsError:    true,
				},
			},
		}
		msgs = append(msgs, errorToolMsg)
		msgs = append(msgs, makeAssistantMsg())
	}

	result := builtinPruneToolResults(msgs)
	// Error results should never be pruned.
	for i, msg := range result {
		if msg.Role == message.Tool {
			tr := msg.Parts[0].(message.ToolResult)
			require.True(t, tr.IsError, "message %d should still be error", i)
			require.Equal(t, bigContent, tr.Content, "error content at %d should be preserved", i)
		}
	}
}

func TestAssistantTurnCutoff(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		makeUserMsg(),      // 0
		makeAssistantMsg(), // 1
		makeToolMsg("a"),   // 2
		makeAssistantMsg(), // 3
		makeUserMsg(),      // 4
		makeAssistantMsg(), // 5
		makeToolMsg("b"),   // 6
		makeAssistantMsg(), // 7
	}
	require.Equal(t, 7, assistantTurnCutoff(msgs, 1))
	require.Equal(t, 5, assistantTurnCutoff(msgs, 2))
	require.Equal(t, 3, assistantTurnCutoff(msgs, 3))
	require.Equal(t, 1, assistantTurnCutoff(msgs, 4))
	require.Equal(t, 0, assistantTurnCutoff(msgs, 5))
}

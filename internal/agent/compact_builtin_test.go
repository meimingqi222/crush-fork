package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func makeToolMsg(content string) message.Message {
	return makeNamedToolMsg("bash", content)
}

func makeNamedToolMsg(name string, content string) message.Message {
	return message.Message{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call-1",
				Name:       name,
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

func makeSummaryMsg() message.Message {
	return message.Message{
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Summary."},
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

func TestBuiltinPruneToolResults_StopsAtSummaryBoundary(t *testing.T) {
	t.Parallel()
	bigContent := strings.Repeat("x", 200_000)

	msgs := []message.Message{
		makeUserMsg(),
		makeAssistantMsg(),
		makeToolMsg("before-summary: " + bigContent),
		makeSummaryMsg(),
		makeUserMsg(),
		makeAssistantMsg(),
		makeToolMsg("after-summary-1: " + bigContent),
		makeAssistantMsg(),
		makeUserMsg(),
		makeAssistantMsg(),
		makeToolMsg("after-summary-2: " + bigContent),
		makeAssistantMsg(),
	}

	result := builtinPruneToolResults(msgs)
	oldTR := result[2].Parts[0].(message.ToolResult)
	require.Contains(t, oldTR.Content, "before-summary")
	require.NotContains(t, oldTR.Content, "Old tool result content cleared")
}

func TestBuiltinPruneToolResults_ProtectsSkillTool(t *testing.T) {
	t.Parallel()
	bigContent := strings.Repeat("x", 200_000)

	var msgs []message.Message
	for i := range 8 {
		msgs = append(msgs, makeUserMsg())
		msgs = append(msgs, makeAssistantMsg())
		if i == 0 {
			msgs = append(msgs, makeNamedToolMsg("skill", "skill-output: "+bigContent))
		} else {
			msgs = append(msgs, makeToolMsg(fmt.Sprintf("large-output-%d: %s", i, bigContent)))
		}
		msgs = append(msgs, makeAssistantMsg())
	}

	result := builtinPruneToolResults(msgs)
	skillTR := result[2].Parts[0].(message.ToolResult)
	require.Contains(t, skillTR.Content, "skill-output")
	require.NotContains(t, skillTR.Content, "Old tool result content cleared")

	olderBashTR := result[6].Parts[0].(message.ToolResult)
	require.Contains(t, olderBashTR.Content, "Old tool result content cleared")
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

func TestBuiltinPruneSkipsCacheProtectedMessages(t *testing.T) {
	t.Parallel()

	longContent := strings.Repeat("x", 50000)
	msgs := []message.Message{
		makeUserMsg(),
		makeAssistantMsg(),
		makeUserMsg(),
		makeAssistantMsg(),
		makeUserMsg(),
		makeAssistantMsg(),
		makeToolMsg(longContent),
	}
	protected := map[int]struct{}{6: {}}
	result := builtinPruneToolResultsWithProtection(msgs, nil, protected)
	tr := result[6].Parts[0].(message.ToolResult)
	require.Equal(t, longContent, tr.Content)
}

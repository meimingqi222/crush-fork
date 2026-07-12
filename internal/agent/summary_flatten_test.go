package agent

import (
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// containsToolParts reports whether any message contains a native tool-call
// or tool-result part.
func containsToolParts(msgs []fantasy.Message) bool {
	for _, msg := range msgs {
		for _, part := range msg.Content {
			switch part.GetType() {
			case fantasy.ContentTypeToolCall, fantasy.ContentTypeToolResult:
				return true
			}
		}
	}
	return false
}

func messageText(msg fantasy.Message) string {
	var b strings.Builder
	for _, part := range msg.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

func TestFlattenToolCallsForSummary(t *testing.T) {
	t.Parallel()

	longInput := `{"pattern":"` + strings.Repeat("x", summaryFlattenMaxToolInputChars+500) + `"}`
	longResult := strings.Repeat("r", summaryFlattenMaxToolResultChars+500)

	input := []fantasy.Message{
		fantasy.NewUserMessage("find all usages of foo"),
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "I should search the codebase."},
				fantasy.TextPart{Text: "Let me search."},
				fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "grep", Input: `{"pattern":"foo"}`},
				fantasy.ToolCallPart{ToolCallID: "call-2", ToolName: "read", Input: longInput},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-1",
					Output:     fantasy.ToolResultOutputContentText{Text: "foo.go:12: foo()"},
				},
				fantasy.ToolResultPart{
					ToolCallID: "call-2",
					Output:     fantasy.ToolResultOutputContentText{Text: longResult},
				},
			},
		},
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call-3", ToolName: "bash", Input: `{"command":"go test"}`},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-3",
					Output:     fantasy.ToolResultOutputContentError{Error: errors.New("exit status 1")},
				},
			},
		},
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "The test fails with exit status 1."},
			},
		},
	}

	out := flattenToolCallsForSummary(input)

	// No native tool parts of any kind may survive.
	require.False(t, containsToolParts(out), "flattened prompt must contain no tool-call/tool-result parts")

	// Message shape: user, assistant, user (results 1+2 merged), assistant,
	// user (error result), assistant.
	require.Len(t, out, 6)
	require.Equal(t, fantasy.MessageRoleUser, out[0].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, out[1].Role)
	require.Equal(t, fantasy.MessageRoleUser, out[2].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, out[3].Role)
	require.Equal(t, fantasy.MessageRoleUser, out[4].Role)
	require.Equal(t, fantasy.MessageRoleAssistant, out[5].Role)

	// The first assistant message keeps its reasoning and text parts and
	// gains textualized tool calls.
	firstAssistant := out[1]
	var sawReasoning bool
	for _, part := range firstAssistant.Content {
		if _, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
			sawReasoning = true
		}
	}
	require.True(t, sawReasoning, "reasoning parts must be preserved")
	firstAssistantText := messageText(firstAssistant)
	require.Contains(t, firstAssistantText, "Let me search.")
	require.Contains(t, firstAssistantText, `[Called grep with input: {"pattern":"foo"}]`)
	require.Contains(t, firstAssistantText, "[Called read with input:")
	require.Contains(t, firstAssistantText, "… (truncated)", "over-long tool input must be truncated")
	require.Less(t, len(firstAssistantText), len(longInput), "flattened text must not embed the full over-long input")

	// The two tool results from one tool message merge into a single user
	// message, with names resolved from the matching tool calls.
	mergedResults := messageText(out[2])
	require.Contains(t, mergedResults, "[Tool result for grep: foo.go:12: foo()]")
	require.Contains(t, mergedResults, "[Tool result for read:")
	require.Contains(t, mergedResults, "… (truncated)", "over-long tool result must be truncated")
	require.Less(t, len(mergedResults), len(longResult), "flattened text must not embed the full over-long result")

	// Error results are labeled as errors.
	require.Contains(t, messageText(out[4]), "[Tool error for bash: exit status 1]")

	// Plain messages pass through untouched.
	require.Equal(t, "find all usages of foo", messageText(out[0]))
	require.Equal(t, "The test fails with exit status 1.", messageText(out[5]))

	// The input must not have been mutated (copy-on-write).
	require.True(t, containsToolParts(input), "input messages must keep their original tool parts")
}

func TestFlattenToolCallsForSummaryPassesThroughToolFreeHistory(t *testing.T) {
	t.Parallel()

	input := []fantasy.Message{
		fantasy.NewSystemMessage("prefix"),
		fantasy.NewUserMessage("hello"),
		{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}},
		},
	}

	out := flattenToolCallsForSummary(input)
	require.Equal(t, input, out)
}

func TestFlattenToolCallsForSummaryHandlesUnknownToolNameAndEmptyInput(t *testing.T) {
	t.Parallel()

	input := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "", Input: ""},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				// A result whose call ID never appeared in an assistant
				// message resolves to the unknown-tool label.
				fantasy.ToolResultPart{
					ToolCallID: "orphan",
					Output:     fantasy.ToolResultOutputContentText{Text: "output"},
				},
			},
		},
	}

	out := flattenToolCallsForSummary(input)
	require.False(t, containsToolParts(out))
	require.Len(t, out, 2)
	require.Equal(t, "[Called unknown tool]", messageText(out[0]))
	require.Equal(t, "[Tool result for unknown tool: output]", messageText(out[1]))
}

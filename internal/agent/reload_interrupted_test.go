package agent

import (
	"testing"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestPreparePromptSessionReloadWithInterruptedToolCalls simulates a process
// crash after an assistant message with tool calls was persisted but before
// any tool results were saved. On session reload, preparePrompt must inject
// synthetic "interrupted" error results so the provider contract holds and the
// model knows to verify state before continuing.
func TestPreparePromptSessionReloadWithInterruptedToolCalls(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	history, _ := a.preparePrompt([]message.Message{
		{
			ID:   "user-1",
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Edit the config file"},
			},
		},
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "I'll read the file first."},
				message.ToolCall{ID: "call-read", Name: agenttools.ReadToolName, Input: `{"path":"config.json"}`, Finished: true},
				message.ToolCall{ID: "call-edit", Name: agenttools.EditToolName, Input: `{"file_path":"config.json","old_string":"a","new_string":"b"}`, Finished: true},
			},
		},
		// NO tool result messages - simulates crash before tool execution completed.
	})

	require.NotEmpty(t, history)

	// Find synthetic tool result parts in the fantasy message history.
	var errorResults []string
	for _, msg := range history {
		for _, part := range msg.Content {
			trPart, ok := part.(fantasy.ToolResultPart)
			if !ok {
				continue
			}
			errOutput, isErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](trPart.Output)
			if isErr {
				require.Contains(t, errOutput.Error.Error(), "interrupted before completion")
				errorResults = append(errorResults, trPart.ToolCallID)
			}
		}
	}

	// Both tool calls should have synthetic interrupted error results.
	require.Len(t, errorResults, 2, "both tool calls should have synthetic interrupted results")
	require.Contains(t, errorResults, "call-read")
	require.Contains(t, errorResults, "call-edit")
}

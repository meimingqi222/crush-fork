package model

import (
	"encoding/json"
	"strings"
	"testing"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestHandleDelayedClickTogglesTaskNodeNestedOperationsOnce(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	chatModel := NewChat(com)
	chatModel.SetSize(120, 20)

	taskNode := chat.NewTaskNodeItem(&theme, "call-general", "task-a", "Task A", "Run task A", "explore", "child-1")

	bashInput, err := json.Marshal(agenttools.BashParams{Command: "echo nested", Description: "nested"})
	require.NoError(t, err)
	nestedTool := chat.NewBashToolMessageItem(&theme, message.ToolCall{
		ID:       "nested-1",
		Name:     agenttools.BashToolName,
		Input:    string(bashInput),
		Finished: true,
	}, &message.ToolResult{ToolCallID: "nested-1", Content: "ok"}, false)
	taskNode.SetNestedTools([]chat.ToolMessageItem{nestedTool})

	chatModel.SetMessages(taskNode)

	collapsed := ansi.Strip(taskNode.Render(120))
	require.Contains(t, collapsed, "▸ 1 operations")
	require.NotContains(t, collapsed, "echo nested")

	handled, _ := chatModel.HandleMouseDown(0, 0)
	require.True(t, handled)

	clicked := chatModel.HandleDelayedClick(DelayedClickMsg{
		ClickID: chatModel.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, clicked)

	expanded := ansi.Strip(taskNode.Render(120))
	require.Contains(t, expanded, "▾ 1 operations")
	require.Contains(t, expanded, "echo nested")
}

func TestHandleDelayedClickExpandsSubtaskResultTool(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	chatModel := NewChat(com)
	chatModel.SetSize(120, 20)

	input, err := json.Marshal(agenttools.SubtaskResultParams{
		SessionID: "child-1",
		Limit:     12000,
	})
	require.NoError(t, err)

	result := "Session: child-1\n\n" + strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5",
		"line 6", "line 7", "line 8", "line 9", "line 10",
		"line 11", "line 12",
	}, "\n")
	item := chat.NewToolMessageItem(&theme, "assistant-1", message.ToolCall{
		ID:       "subtask-result-1",
		Name:     agenttools.SubtaskResultToolName,
		Input:    string(input),
		Finished: true,
	}, &message.ToolResult{ToolCallID: "subtask-result-1", Content: result}, false)
	chatModel.SetMessages(item)

	collapsed := ansi.Strip(item.Render(120))
	require.Contains(t, collapsed, "lines hidden")
	require.NotContains(t, collapsed, "line 12")

	handled, _ := chatModel.HandleMouseDown(0, 0)
	require.True(t, handled)

	clicked := chatModel.HandleDelayedClick(DelayedClickMsg{
		ClickID: chatModel.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, clicked)

	expanded := ansi.Strip(item.Render(120))
	require.Contains(t, expanded, "line 12")
	require.NotContains(t, expanded, "lines hidden")
}

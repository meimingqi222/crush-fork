package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestAgentToolMessageItemRendersSubagentTypeAndDescription(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Description:  "Implement parser worker",
		Prompt:       "Update the parser package and run targeted tests",
		SubagentType: "general",
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-1",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-1",
		Content:    "done",
	}, false)

	rendered := item.Render(80)
	require.Contains(t, rendered, "General")
	require.Contains(t, rendered, "Implement parser worker")
	require.Contains(t, rendered, "Update the parser package and run targeted tests")
}

func TestAgentToolMessageItemRendersTaskListForTaskGraph(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Tasks: []agent.AgentTaskParams{
			{Name: "t1", Description: "Search references", Assignment: "Find usages", SubagentType: "explore"},
			{Name: "t2", Description: "Apply patch", Assignment: "Implement fix", SubagentType: "general"},
			{Name: "t3", Description: "Run tests", Assignment: "Run targeted tests", SubagentType: "general"},
			{Name: "t4", Description: "Summarize", Assignment: "Write summary", SubagentType: "general"},
		},
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-taskgraph",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-taskgraph",
		Content:    "done",
	}, false)

	rendered := ansi.Strip(item.Render(120))
	require.Contains(t, rendered, "Tasks")
	require.Contains(t, rendered, "done 0 · running 0 · pending 4")
	require.Contains(t, rendered, "[Explore] Search references")
	require.Contains(t, rendered, "[General] Apply patch")
	require.Contains(t, rendered, "[General] Run tests")
	require.Contains(t, rendered, "[General] Summarize")
}

func TestAgentToolMessageItemRendersChildSessionStatus(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Description:  "Implement parser worker",
		Prompt:       "Update the parser package and run targeted tests",
		SubagentType: "general",
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:    "tool-1",
		Name:  agent.AgentToolName,
		Input: string(params),
	}, nil, false)
	item.SetChildSessionStatus("Service temporarily unavailable. Retrying in 3 seconds... (attempt 1/5)", false)

	rendered := ansi.Strip(item.Render(120))
	require.Contains(t, rendered, "Status")
	require.Contains(t, rendered, "Retrying in 3 seconds")
}

func TestAgentToolMessageItemRendersTaskStatuses(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Tasks: []agent.AgentTaskParams{
			{Name: "t1", Description: "Index code", Assignment: "Build map", SubagentType: "explore"},
			{Name: "t2", Description: "Apply fix", Assignment: "Patch", SubagentType: "general"},
		},
	})
	require.NoError(t, err)

	resultContent := "All good\n- t1: completed\n- t2: failed\n\nTask outputs:\n- t1 ..."
	result := message.ToolResult{
		ToolCallID: "tool-graph",
		Content:    resultContent,
	}.WithReducer(message.ToolResultReducer{
		ChildSessions: []message.ToolResultReducerChildSession{
			{TaskID: "t1", Status: message.ToolResultSubtaskStatusCompleted},
			{TaskID: "t2", Status: message.ToolResultSubtaskStatusFailed},
		},
	})
	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-graph",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, &result, false)

	rendered := ansi.Strip(item.Render(140))
	require.Contains(t, rendered, "done 1 · running 0 · pending 0 · failed 1")
	require.Contains(t, rendered, "[Explore] Index code")
	require.Contains(t, rendered, "[General] Apply fix")
}

func TestAgentToolMessageItemRendersCompletedWarningsStatuses(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Tasks: []agent.AgentTaskParams{
			{Name: "t1", Description: "Collect data", Assignment: "Inspect logs", SubagentType: "explore"},
			{Name: "t2", Description: "Patch config", Assignment: "Update settings", SubagentType: "general"},
		},
	})
	require.NoError(t, err)

	resultContent := "Summary\n- t1: completed_with_warnings\n- t2: completed"
	result := message.ToolResult{
		ToolCallID: "tool-warn-blocked",
		Content:    resultContent,
	}.WithReducer(message.ToolResultReducer{
		ChildSessions: []message.ToolResultReducerChildSession{
			{TaskID: "t1", Status: message.ToolResultSubtaskStatusCompletedWithWarnings},
			{TaskID: "t2", Status: message.ToolResultSubtaskStatusCompleted},
		},
	})
	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-warn-blocked",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, &result, false)

	rendered := ansi.Strip(item.Render(140))
	require.Contains(t, rendered, "done 2 · running 0 · pending 0")
	require.Contains(t, rendered, "[Explore] Collect data")
	require.Contains(t, rendered, "[General] Patch config")
}

func TestAgentToolMessageItemRendersMixedTaskStatusCounts(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Tasks: []agent.AgentTaskParams{
			{Name: "t1", Description: "Index code", Assignment: "Build map", SubagentType: "explore"},
			{Name: "t2", Description: "Apply fix", Assignment: "Patch", SubagentType: "general"},
			{Name: "t3", Description: "Run tests", Assignment: "Run targeted tests", SubagentType: "general"},
			{Name: "t4", Description: "Summarize", Assignment: "Write summary", SubagentType: "general"},
			{Name: "t5", Description: "Blocked task", Assignment: "Wait", SubagentType: "general"},
		},
	})
	require.NoError(t, err)

	result := message.ToolResult{
		ToolCallID: "tool-mixed",
		Content:    "mixed",
	}.WithReducer(message.ToolResultReducer{
		ChildSessions: []message.ToolResultReducerChildSession{
			{TaskID: "t1", Status: message.ToolResultSubtaskStatusCompleted},
			{TaskID: "t2", Status: message.ToolResultSubtaskStatusInProgress},
			{TaskID: "t3", Status: message.ToolResultSubtaskStatusRunning},
			{TaskID: "t4", Status: message.ToolResultSubtaskStatusPending},
			{TaskID: "t5", Status: message.ToolResultSubtaskStatusBlocked},
		},
	})
	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-mixed",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, &result, false)

	rendered := ansi.Strip(item.Render(140))
	require.Contains(t, rendered, "done 1 · running 2 · pending 1 · blocked 1")
	require.Contains(t, rendered, "[Explore] Index code")
	require.Contains(t, rendered, "[General] Apply fix")
	require.Contains(t, rendered, "[General] Run tests")
	require.Contains(t, rendered, "[General] Summarize")
	require.Contains(t, rendered, "[General] Blocked task")
}

func TestAgentToolMessageItemCollapsesNestedToolsByDefault(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agent.AgentParams{
		Description:  "Long review",
		Prompt:       "Inspect recent commits",
		SubagentType: "explore",
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewAgentToolMessageItem(&theme, message.ToolCall{
		ID:       "agent-tool",
		Name:     agent.AgentToolName,
		Input:    string(params),
		Finished: true,
	}, nil, false)

	for i := 1; i <= 12; i++ {
		item.AddNestedTool(newNestedBashTool(t, &theme, fmt.Sprintf("nested-%02d", i)))
	}

	collapsed := ansi.Strip(item.Render(140))
	require.Contains(t, collapsed, "Expand (2 more)")
	require.Contains(t, collapsed, "nested-10")
	require.NotContains(t, collapsed, "nested-11")
	require.NotContains(t, collapsed, "nested-12")

	item.ToggleExpanded()
	expanded := ansi.Strip(item.Render(140))
	require.Contains(t, expanded, "Collapse")
	require.Contains(t, expanded, "nested-11")
	require.Contains(t, expanded, "nested-12")
}

func TestAgenticFetchToolMessageItemCollapsesNestedToolsByDefault(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(agenticFetchParams{
		Prompt: "Collect package docs",
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewAgenticFetchToolMessageItem(&theme, message.ToolCall{
		ID:       "agentic-fetch-tool",
		Name:     agenttools.AgenticFetchToolName,
		Input:    string(params),
		Finished: true,
	}, nil, false)

	for i := 1; i <= 11; i++ {
		item.AddNestedTool(newNestedBashTool(t, &theme, fmt.Sprintf("fetch-nested-%02d", i)))
	}

	collapsed := ansi.Strip(item.Render(140))
	require.Contains(t, collapsed, "Expand (1 more)")
	require.NotContains(t, collapsed, "fetch-nested-11")

	item.ToggleExpanded()
	expanded := ansi.Strip(item.Render(140))
	require.Contains(t, expanded, "Collapse")
	require.Contains(t, expanded, "fetch-nested-11")
}

func TestTaskNodeItemRendersCompletedWarningsAndBlockedIcons(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	warningNode := NewTaskNodeItem(&theme, "parent", "warn", "Collect data", "Inspect logs", "explore", "child-1")
	warningNode.SetCompletionStatus(message.ToolResultSubtaskStatus("completed_with_warnings"))
	warningRendered := warningNode.RawRender(100)
	require.Contains(t, warningRendered, theme.Tool.IconSuccess.String())

	blockedNode := NewTaskNodeItem(&theme, "parent", "blocked", "Patch config", "Update settings", "general", "child-2")
	blockedNode.SetCompletionStatus(message.ToolResultSubtaskStatus("blocked"))
	blockedRendered := blockedNode.RawRender(100)
	require.Contains(t, blockedRendered, theme.Tool.IconError.String())
}

func newNestedBashTool(t *testing.T, sty *styles.Styles, cmd string) ToolMessageItem {
	t.Helper()

	input, err := json.Marshal(agenttools.BashParams{Command: cmd, Description: "nested"})
	require.NoError(t, err)

	return NewBashToolMessageItem(sty, message.ToolCall{
		ID:       "nested-" + cmd,
		Name:     agenttools.BashToolName,
		Input:    string(input),
		Finished: true,
	}, nil, false)
}

func TestBashToolMessageItemRuntimeSnapshotRendersSanitizedText(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal(agenttools.BashParams{Command: "echo test", Description: "runtime"})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewBashToolMessageItem(&theme, message.ToolCall{
		ID:    "tool-runtime",
		Name:  agenttools.BashToolName,
		Input: string(input),
	}, nil, false)
	item.SetRuntimeState(&toolruntime.State{
		ToolCallID:   "tool-runtime",
		ToolName:     agenttools.BashToolName,
		Status:       toolruntime.StatusRunning,
		SnapshotText: "3\nwarn",
	})

	rendered := ansi.Strip(item.Render(100))
	require.Contains(t, rendered, "3")
	require.Contains(t, rendered, "warn")
	require.NotContains(t, rendered, "Waiting for tool response...")
	require.NotContains(t, rendered, "\x1b")
}

func TestBashToolMessageItemFinalRuntimeSnapshotRendersBeforeToolResultArrives(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal(agenttools.BashParams{Command: "git show --stat HEAD", Description: "runtime final"})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewBashToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-runtime-final",
		Name:     agenttools.BashToolName,
		Input:    string(input),
		Finished: true,
	}, nil, false)
	item.SetRuntimeState(&toolruntime.State{
		ToolCallID:   "tool-runtime-final",
		ToolName:     agenttools.BashToolName,
		Status:       toolruntime.StatusCompleted,
		SnapshotText: "commit abc123\n file.go | 2 +-",
	})

	rendered := ansi.Strip(item.Render(100))
	require.Contains(t, rendered, "commit abc123")
	require.Contains(t, rendered, "file.go | 2 +-")
	require.NotContains(t, rendered, "Waiting for tool response...")
	require.NotContains(t, rendered, "no output")
	if bashItem, ok := item.(*BashToolMessageItem); ok {
		require.Equal(t, ToolStatusSuccess, bashItem.computeStatus())
	}
}

func TestBashToolMessageItemRuntimeFailedOverridesNonErrorResult(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal(agenttools.BashParams{Command: "git diff", Description: "diff"})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewBashToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-runtime-failed",
		Name:     agenttools.BashToolName,
		Input:    string(input),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-runtime-failed",
		Name:       agenttools.BashToolName,
		Content:    "no changes",
		IsError:    false,
	}, false)
	item.SetRuntimeState(&toolruntime.State{
		ToolCallID:   "tool-runtime-failed",
		ToolName:     agenttools.BashToolName,
		Status:       toolruntime.StatusFailed,
		SnapshotText: "exit code 1",
	})

	if bashItem, ok := item.(*BashToolMessageItem); ok {
		require.Equal(t, ToolStatusError, bashItem.computeStatus())
	}

	rendered := ansi.Strip(item.Render(100))
	require.Contains(t, rendered, styles.ToolError)
	require.Contains(t, rendered, "exit code 1")
	require.NotContains(t, rendered, styles.ToolSuccess)
}

func TestBashToolMessageItemFailedResultShowsFullOutputPreview(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal(agenttools.BashParams{Command: "go test ./internal/ui/model", Description: "test"})
	require.NoError(t, err)

	failure := strings.Join([]string{
		"--- FAIL: TestExample (2.11s)",
		"    testing.go:1464: TempDir RemoveAll cleanup: unlinkat C:/tmp/state/logs/crush.log: The process cannot access the file because it is being used by another process.",
		"FAIL",
	}, "\n")
	meta, err := json.Marshal(agenttools.BashResponseMetadata{Output: failure})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewBashToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-failed-result",
		Name:     agenttools.BashToolName,
		Input:    string(input),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-failed-result",
		Name:       agenttools.BashToolName,
		Content:    failure,
		Metadata:   string(meta),
		IsError:    true,
	}, false)

	rendered := ansi.Strip(item.Render(160))
	require.Contains(t, rendered, styles.ToolError)
	require.Contains(t, rendered, "TempDir RemoveAll cleanup")
	require.Contains(t, rendered, "crush.log")
}

func TestBashToolMessageItemFinalResultPrefersResultOverStaleRuntimeSnapshot(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal(agenttools.BashParams{Command: "printf final", Description: "final"})
	require.NoError(t, err)
	meta, err := json.Marshal(agenttools.BashResponseMetadata{Output: "final output"})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	item := NewBashToolMessageItem(&theme, message.ToolCall{
		ID:       "tool-final-result",
		Name:     agenttools.BashToolName,
		Input:    string(input),
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "tool-final-result",
		Name:       agenttools.BashToolName,
		Content:    "final output",
		Metadata:   string(meta),
	}, false)
	item.SetRuntimeState(&toolruntime.State{
		ToolCallID:   "tool-final-result",
		ToolName:     agenttools.BashToolName,
		Status:       toolruntime.StatusCompleted,
		SnapshotText: "stale partial output",
	})

	rendered := ansi.Strip(item.Render(100))
	require.Contains(t, rendered, "final output")
	require.NotContains(t, rendered, "stale partial output")
}

// TestAssistantMessageDoesNotRenderProposedPlanHeader verifies that assistant
// messages never render an inline "Proposed Plan" header, regardless of
// whether the message also carries a resolve tool call. Proposed-plan review
// is handled out-of-band by the file-based plan-review dialog (see
// planReviewLoadedMsg / dialog.NewPlanReview), not by inline message
// rendering, so both cases below must omit the header while still rendering
// the message's own text content.
func TestAssistantMessageDoesNotRenderProposedPlanHeader(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	content := "The plan is ready for review."

	withoutResolve := message.Message{
		ID:   "assistant-without-resolve",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: content},
		},
	}
	item := NewAssistantMessageItem(&theme, &withoutResolve)
	rendered := ansi.Strip(item.Render(120))
	require.NotContains(t, rendered, "Proposed Plan")
	require.Contains(t, rendered, content)

	withResolve := message.Message{
		ID:   "assistant-with-resolve",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: content},
			message.ToolCall{
				ID:       "tool-1",
				Name:     agenttools.ResolveToolName,
				Input:    `{"action":"apply","reason":"plan is ready","extra":{"title":"auth-refactor"}}`,
				Finished: true,
			},
		},
	}
	item = NewAssistantMessageItem(&theme, &withResolve)
	rendered = ansi.Strip(item.Render(120))
	require.NotContains(t, rendered, "Proposed Plan")
	require.Contains(t, rendered, content)
}

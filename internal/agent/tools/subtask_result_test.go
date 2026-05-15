package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/stretchr/testify/require"
)

type subtaskResultMessageStub struct {
	*pubsub.Broker[message.Message]
	bySession map[string][]message.Message
}

func newSubtaskResultMessageStub() *subtaskResultMessageStub {
	return &subtaskResultMessageStub{
		Broker:    pubsub.NewBroker[message.Message](),
		bySession: make(map[string][]message.Message),
	}
}

func (s *subtaskResultMessageStub) Create(context.Context, string, message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, nil
}

func (s *subtaskResultMessageStub) Update(context.Context, message.Message) error { return nil }

func (s *subtaskResultMessageStub) Get(context.Context, string) (message.Message, error) {
	return message.Message{}, nil
}

func (s *subtaskResultMessageStub) List(_ context.Context, sessionID string) ([]message.Message, error) {
	return append([]message.Message(nil), s.bySession[sessionID]...), nil
}

func (s *subtaskResultMessageStub) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (s *subtaskResultMessageStub) ListAllUserMessages(context.Context) ([]message.Message, error) {
	return nil, nil
}

func (s *subtaskResultMessageStub) Delete(context.Context, string) error { return nil }

func (s *subtaskResultMessageStub) DeleteSessionMessages(context.Context, string) error { return nil }

func runSubtaskResultTool(
	t *testing.T,
	ctx context.Context,
	tool fantasy.AgentTool,
	params SubtaskResultParams,
) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	return tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  SubtaskResultToolName,
		Input: string(input),
	})
}

func TestSubtaskResultToolInfersLatestChildSessionWhenSessionIDOmitted(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	childToolResult := message.ToolResult{
		ToolCallID: "agent-call-1",
		Name:       "agent",
		Content:    "truncated summary",
	}.WithSubtaskResult(message.ToolResultSubtaskResult{
		ChildSessionID:   "child-session-1",
		ParentToolCallID: "agent-call-1",
		ParentMessageID:  "msg-parent-1",
		Status:           message.ToolResultSubtaskStatusCompleted,
	})

	stub.bySession["parent-session-1"] = []message.Message{
		{
			ID:   "tool-msg-1",
			Role: message.Tool,
			Parts: []message.ContentPart{
				childToolResult,
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
	}
	stub.bySession["child-session-1"] = []message.Message{
		{
			ID:   "assistant-msg-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "full child result"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session-1")
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Session: child-session-1")
	require.Contains(t, resp.Content, "full child result")
}

func TestSubtaskResultToolResolvesLiteralPlaceholderToLatestChildSession(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	childToolResult := message.ToolResult{
		ToolCallID: "agent-call-2",
		Name:       "agent",
		Content:    "truncated summary",
	}.WithSubtaskResult(message.ToolResultSubtaskResult{
		ChildSessionID:   "child-session-2",
		ParentToolCallID: "agent-call-2",
		ParentMessageID:  "msg-parent-2",
		Status:           message.ToolResultSubtaskStatusCompleted,
	})

	stub.bySession["parent-session-2"] = []message.Message{
		{
			ID:   "tool-msg-2",
			Role: message.Tool,
			Parts: []message.ContentPart{
				childToolResult,
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
	}
	stub.bySession["child-session-2"] = []message.Message{
		{
			ID:   "assistant-msg-2",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "resolved child output"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session-2")
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{
		SessionID: "messageID$$toolCallID",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Session: child-session-2")
	require.Contains(t, resp.Content, "resolved child output")
}

func TestSubtaskResultToolReportsInferenceFailureClearly(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session-3")
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no child session could be inferred")
}

func TestSubtaskResultToolFallsBackToBackgroundAgentWhenSessionIDHasNoAssistantText(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	stub.bySession["agent-1"] = []message.Message{}
	stub.bySession["child-session-1"] = []message.Message{
		{
			ID:   "assistant-msg-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "full background result"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	ctx := toolruntime.WithBackgroundAgentLookup(context.Background(), func(agentID string) (string, string, string, bool) {
		require.Equal(t, "agent-1", agentID)
		return "completed", "cached background result", "child-session-1", true
	})
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{
		SessionID: "agent-1",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `Agent "agent-1" (completed):`)
	require.Contains(t, resp.Content, "full background result")
	require.NotContains(t, resp.Content, "No assistant response found")
}

func TestSubtaskResultToolInfersLatestReducerChildSession(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	reducerResult := message.ToolResult{
		ToolCallID: "agent-call-1",
		Name:       "agent",
		Content:    "Completed 2/2 subtasks.",
	}.WithReducer(message.ToolResultReducer{
		Summary: "Completed 2/2 subtasks.",
		ChildSessions: []message.ToolResultReducerChildSession{
			{TaskID: "a", SessionID: "child-session-a", Status: message.ToolResultSubtaskStatusCompleted},
			{TaskID: "b", SessionID: "child-session-b", Status: message.ToolResultSubtaskStatusCompleted},
		},
	})

	stub.bySession["parent-session-4"] = []message.Message{
		{
			ID:   "tool-msg-1",
			Role: message.Tool,
			Parts: []message.ContentPart{
				reducerResult,
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
	}
	stub.bySession["child-session-b"] = []message.Message{
		{
			ID:   "assistant-msg-b",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "latest reducer child result"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session-4")
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Session: child-session-b")
	require.Contains(t, resp.Content, "latest reducer child result")
}

func TestSubtaskResultToolResolvesTaskRef(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	reducerResult := message.ToolResult{
		ToolCallID: "agent-call-1",
		Name:       "agent",
		Content:    "Task output: subtask://0-review",
	}.WithReducer(message.ToolResultReducer{
		Summary: "Completed 1/1 subtasks.",
		ChildSessions: []message.ToolResultReducerChildSession{
			{
				TaskID:        "very-long-review-task-id-that-the-model-should-not-need",
				TaskRef:       "0-review",
				SessionID:     "child-session-review",
				Status:        message.ToolResultSubtaskStatusCompleted,
				Preview:       "preview text",
				OutputChars:   12,
				HasFullOutput: true,
			},
		},
	})

	stub.bySession["parent-session-task-ref"] = []message.Message{
		{
			ID:   "tool-msg-1",
			Role: message.Tool,
			Parts: []message.ContentPart{
				reducerResult,
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
	}
	stub.bySession["child-session-review"] = []message.Message{
		{
			ID:   "assistant-msg-review",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "full review result"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "parent-session-task-ref")
	resp, err := runSubtaskResultTool(t, ctx, tool, SubtaskResultParams{TaskRef: "subtask://0-review"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Session: child-session-review")
	require.Contains(t, resp.Content, "Task ref: 0-review")
	require.Contains(t, resp.Content, "full review result")
}

func TestSubtaskResultToolFallsBackToSubagentFinishMetadata(t *testing.T) {
	t.Parallel()

	stub := newSubtaskResultMessageStub()
	tool := NewSubtaskResultTool(stub)

	stub.bySession["child-session-finish"] = []message.Message{
		{
			ID:   "tool-msg-finish",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{
					ToolCallID: "finish-call",
					Name:       "subagent_finish",
					Content:    "",
				}.WithSubagentFinish(message.ToolResultSubagentFinish{
					Status:       message.ToolResultSubtaskStatusCompletedWithWarnings,
					Summary:      "structured finish summary",
					FilesTouched: []string{"internal/a.go"},
					TestResults:  []string{"go test ./internal/agent"},
				}),
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
	}

	resp, err := runSubtaskResultTool(t, context.Background(), tool, SubtaskResultParams{
		SessionID: "child-session-finish",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Session: child-session-finish")
	require.Contains(t, resp.Content, "structured finish summary")
	require.Contains(t, resp.Content, "internal/a.go")
	require.NotContains(t, resp.Content, "No assistant response found")
}

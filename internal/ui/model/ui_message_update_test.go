package model

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestUpdateSessionMessageReinsertsAssistantAfterToolOnly(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:  com,
		chat: NewChat(com),
	}

	assistantMsg := message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
	}
	ui.chat.AppendMessages(chat.NewAssistantMessageItem(ui.com.Styles, &assistantMsg))
	require.NotNil(t, ui.chat.MessageItem(assistantMsg.ID))

	// First update: assistant message becomes tool-only; UI removes the assistant item.
	assistantMsg.Parts = append(assistantMsg.Parts, message.ToolCall{
		ID:       "tool-1",
		Name:     "bash",
		Finished: true,
	})
	_ = ui.updateSessionMessage(assistantMsg)
	require.Nil(t, ui.chat.MessageItem(assistantMsg.ID))
	require.NotNil(t, ui.chat.MessageItem("tool-1"))

	// Second update: same assistant message gets text content; UI should re-insert it.
	assistantMsg.Parts = append(assistantMsg.Parts, message.TextContent{Text: "Hello"})
	_ = ui.updateSessionMessage(assistantMsg)

	require.NotNil(t, ui.chat.MessageItem(assistantMsg.ID))
	require.NotNil(t, ui.chat.MessageItem("tool-1"))
	require.Less(t, ui.chat.idInxMap[assistantMsg.ID], ui.chat.idInxMap["tool-1"])
}

func TestUpdateSessionMessageRemovesStaleToolItemsAfterRetryReset(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:  com,
		chat: NewChat(com),
	}

	assistantMsg := message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "tool-1",
				Name:     "write",
				Input:    `{"file_path":"retry.txt","content":"before"}`,
				Finished: true,
			},
		},
	}
	_ = ui.updateSessionMessage(assistantMsg)
	require.NotNil(t, ui.chat.MessageItem("tool-1"))

	assistantMsg.Parts = nil
	_ = ui.updateSessionMessage(assistantMsg)

	require.Nil(t, ui.chat.MessageItem("tool-1"))
}

func TestDeletedAssistantMessageRemovesAssociatedToolItems(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:     com,
		chat:    NewChat(com),
		session: &session.Session{ID: "session-1"},
	}

	toolCall := message.ToolCall{
		ID:       "tool-1",
		Name:     "write",
		Input:    `{"file_path":"retry.txt","content":"before"}`,
		Finished: true,
	}
	ui.chat.SetMessages(chat.NewToolMessageItem(ui.com.Styles, "assistant-1", toolCall, nil, false))
	require.NotNil(t, ui.chat.MessageItem("tool-1"))

	_, _ = ui.Update(pubsub.Event[message.Message]{
		Type: pubsub.DeletedEvent,
		Payload: message.Message{
			ID:        "assistant-1",
			SessionID: "session-1",
			Role:      message.Assistant,
			Parts:     []message.ContentPart{toolCall},
		},
	})

	require.Nil(t, ui.chat.MessageItem("tool-1"))
}

func TestShouldRefreshSessionUsage(t *testing.T) {
	t.Parallel()

	ui := &UI{}
	msg := message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "done"},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: 100},
		},
	}

	require.True(t, ui.shouldRefreshSessionUsage(pubsub.UpdatedEvent, msg))
	require.True(t, ui.shouldRefreshSessionUsage(pubsub.UpdatedEvent, msg))

	changed := msg
	changed.Parts = []message.ContentPart{
		message.TextContent{Text: "done!"},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: 100},
	}
	require.True(t, ui.shouldRefreshSessionUsage(pubsub.UpdatedEvent, changed))
	require.False(t, ui.shouldRefreshSessionUsage(pubsub.CreatedEvent, changed))

	unfinished := message.Message{ID: "assistant-2", Role: message.Assistant}
	require.False(t, ui.shouldRefreshSessionUsage(pubsub.UpdatedEvent, unfinished))
}

func TestSetSessionMessagesSuppressesStaleLoadingStateForRestoredSession(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:     com,
		chat:    NewChat(com),
		session: &session.Session{ID: "session-1"},
	}

	cmd := ui.setSessionMessages([]message.Message{
		{
			ID:   "assistant-1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "still thinking"},
			},
		},
		{
			ID:   "assistant-2",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:       "tool-1",
					Name:     "bash",
					Input:    `{"command":"sleep 10"}`,
					Finished: false,
				},
			},
		},
	})
	_ = cmd

	assistantItem := ui.chat.MessageItem("assistant-1")
	require.NotNil(t, assistantItem)
	assistantRendered := ansi.Strip(assistantItem.Render(80))
	require.Contains(t, assistantRendered, "still thinking")
	require.NotContains(t, assistantRendered, "Thinking")

	toolItem := ui.chat.MessageItem("tool-1")
	require.NotNil(t, toolItem)
	toolRendered := ansi.Strip(toolItem.Render(80))
	require.Contains(t, toolRendered, "Bash")
	require.NotContains(t, toolRendered, "Waiting for tool response...")
}

func TestStopStaleLoadingIndicatorsStopsAgentSpinnerWhenRunAlreadyEnded(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:     com,
		chat:    NewChat(com),
		session: &session.Session{ID: "session-1"},
	}

	msg := message.Message{
		ID:   "assistant-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "still thinking"},
		},
	}
	ui.chat.SetMessages(chat.ExtractMessageItems(ui.com.Styles, &msg, nil)...)

	assistantItem := ui.chat.MessageItem("assistant-1")
	require.NotNil(t, assistantItem)
	before := ansi.Strip(assistantItem.Render(100))
	require.Contains(t, before, "Thinking")

	ui.stopStaleLoadingIndicators()

	after := ansi.Strip(assistantItem.Render(100))
	require.NotContains(t, after, "Thinking")
	require.Contains(t, after, "still thinking")
}

func TestHandleChildSessionMessageShowsAndClearsRetryStatus(t *testing.T) {
	t.Parallel()

	ui, parent, generalChild, _, _, _ := testSessionUI(t)
	ui.session = parent

	msgs, err := ui.com.App.Messages.List(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	toolCalls := msgs[0].ToolCalls()
	require.NotEmpty(t, toolCalls)
	ui.chat.SetMessages(chat.NewToolMessageItem(ui.com.Styles, msgs[0].ID, toolCalls[0], nil, false))

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "child-retry-1",
			SessionID: generalChild.ID,
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Service temporarily unavailable. Retrying in 3 seconds... (attempt 1/5)"},
			},
		},
	})

	rendered := ansi.Strip(ui.chat.MessageItem(toolCalls[0].ID).Render(100))
	require.Contains(t, rendered, "Retrying in 3 seconds")

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "child-assistant-2",
			SessionID: generalChild.ID,
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Final child answer"},
			},
		},
	})

	rendered = ansi.Strip(ui.chat.MessageItem(toolCalls[0].ID).Render(100))
	require.NotContains(t, rendered, "Retrying in 3 seconds")
}

func TestHandleChildSessionMessageClearsRetryStatusOnDelete(t *testing.T) {
	t.Parallel()

	ui, parent, generalChild, _, _, _ := testSessionUI(t)
	ui.session = parent

	msgs, err := ui.com.App.Messages.List(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	toolCalls := msgs[0].ToolCalls()
	require.NotEmpty(t, toolCalls)
	ui.chat.SetMessages(chat.NewToolMessageItem(ui.com.Styles, msgs[0].ID, toolCalls[0], nil, false))

	retryMsg := message.Message{
		ID:        "child-retry-1",
		SessionID: generalChild.ID,
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Service temporarily unavailable. Retrying in 3 seconds... (attempt 1/5)"},
		},
	}

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: retryMsg,
	})

	rendered := ansi.Strip(ui.chat.MessageItem(toolCalls[0].ID).Render(100))
	require.Contains(t, rendered, "Retrying in 3 seconds")

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type:    pubsub.DeletedEvent,
		Payload: retryMsg,
	})

	rendered = ansi.Strip(ui.chat.MessageItem(toolCalls[0].ID).Render(100))
	require.NotContains(t, rendered, "Retrying in 3 seconds")
}

func TestMaybeOpenProposedPlanDialogRequiresPlanExit(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:     com,
		dialog:  dialog.NewOverlay(),
		session: &session.Session{ID: "session-1", CollaborationMode: session.CollaborationModePlan},
	}

	msg := message.Message{
		ID:        "assistant-1",
		SessionID: "session-1",
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: planmode.WrapProposedPlan("- Step 1")},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: 1},
		},
	}

	require.Nil(t, ui.maybeOpenProposedPlanDialog(msg))
	require.False(t, ui.dialog.ContainsDialog(dialog.ProposedPlanID))

	msg.Parts = append(msg.Parts,
		message.ToolCall{ID: "tool-1", Name: agenttools.PlanExitToolName, Finished: true},
	)

	require.Nil(t, ui.maybeOpenProposedPlanDialog(msg))
	require.True(t, ui.dialog.ContainsDialog(dialog.ProposedPlanID))
}

func TestHandleChildSessionMessageRemovesStaleNestedToolsAfterRetryReset(t *testing.T) {
	t.Parallel()

	ui, parent, generalChild, _, _, _ := testSessionUI(t)
	ui.session = parent

	msgs, err := ui.com.App.Messages.List(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	toolCalls := msgs[0].ToolCalls()
	require.NotEmpty(t, toolCalls)
	ui.chat.SetMessages(chat.NewToolMessageItem(ui.com.Styles, msgs[0].ID, toolCalls[0], nil, false))

	childAssistant := message.Message{
		ID:        "child-assistant-1",
		SessionID: generalChild.ID,
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "child-write-1",
				Name:     "write",
				Input:    `{"file_path":"retry.txt","content":"before"}`,
				Finished: false,
			},
		},
	}

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: childAssistant,
	})

	parentTool, ok := ui.chat.MessageItem(toolCalls[0].ID).(chat.NestedToolContainer)
	require.True(t, ok)
	require.Len(t, parentTool.NestedTools(), 1)

	childAssistant.Parts = nil
	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type:    pubsub.UpdatedEvent,
		Payload: childAssistant,
	})

	parentTool, ok = ui.chat.MessageItem(toolCalls[0].ID).(chat.NestedToolContainer)
	require.True(t, ok)
	require.Empty(t, parentTool.NestedTools())
}

func TestHandleChildSessionMessageMapsTaskGraphRetrySessionsToTaskNode(t *testing.T) {
	t.Parallel()

	ui, parent, generalChild, _, _, _ := testSessionUI(t)
	ui.session = parent

	msgs, err := ui.com.App.Messages.List(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	toolCalls := msgs[0].ToolCalls()
	require.NotEmpty(t, toolCalls)
	taskSessionID := ui.com.App.Sessions.CreateAgentToolSessionID(msgs[0].ID, "call-general::task-a")
	ui.chat.SetMessages(
		chat.NewToolMessageItem(ui.com.Styles, msgs[0].ID, toolCalls[0], nil, false),
		chat.NewTaskNodeItem(ui.com.Styles, toolCalls[0].ID, "task-a", "Task A", "Run task A", "general", taskSessionID),
	)

	retryChildSessionID := ui.com.App.Sessions.CreateAgentToolSessionID(msgs[0].ID, "call-general::task-a::retry-1")
	retryChild, err := ui.com.App.Sessions.CreateTaskSession(t.Context(), retryChildSessionID, parent.ID, "Retry child")
	require.NoError(t, err)
	require.Equal(t, generalChild.ParentSessionID, retryChild.ParentSessionID)

	retryEvent := message.Message{
		ID:        "retry-assistant-1",
		SessionID: retryChild.ID,
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "retry-nested-1",
				Name:     "view",
				Input:    `{"file_path":"README.md"}`,
				Finished: false,
			},
		},
	}

	_ = ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: retryEvent,
	})

	taskNode, ok := ui.chat.MessageItem(chat.TaskNodeItemID(toolCalls[0].ID, "task-a")).(chat.NestedToolContainer)
	require.True(t, ok)
	require.Len(t, taskNode.NestedTools(), 1)
	require.Equal(t, "retry-nested-1", taskNode.NestedTools()[0].ToolCall().ID)

	parentTool, ok := ui.chat.MessageItem(toolCalls[0].ID).(chat.NestedToolContainer)
	require.True(t, ok)
	require.Empty(t, parentTool.NestedTools())
}

func TestSetSessionMessagesLoadsTaskNodeNestedToolsFromRetrySessions(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	fileTracker := filetracker.NewService(q)
	historyService := history.NewService(q, conn)

	parent, err := sessions.Create(context.Background(), "Parent")
	require.NoError(t, err)

	assistantMsg, err := messages.Create(context.Background(), parent.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:   "call-general",
				Name: agent.AgentToolName,
				Input: `{"tasks":[{"id":"task-a","description":"Search references","prompt":"Find usages","subagent_type":"explore"},` +
					`{"id":"task-b","description":"Apply patch","prompt":"Implement fix","subagent_type":"general"}]}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	baseTaskSessionID := sessions.CreateAgentToolSessionID(assistantMsg.ID, "call-general::task-a")
	baseTaskSession, err := sessions.CreateTaskSession(context.Background(), baseTaskSessionID, parent.ID, "Task A")
	require.NoError(t, err)

	retryTaskSessionID := sessions.CreateAgentToolSessionID(assistantMsg.ID, "call-general::task-a::retry-1")
	retryTaskSession, err := sessions.CreateTaskSession(context.Background(), retryTaskSessionID, parent.ID, "Task A retry")
	require.NoError(t, err)

	_, err = messages.Create(context.Background(), baseTaskSession.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "base-view", Name: "view", Input: `{"file_path":"a.txt"}`, Finished: true},
		},
	})
	require.NoError(t, err)

	_, err = messages.Create(context.Background(), retryTaskSession.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "retry-view", Name: "view", Input: `{"file_path":"b.txt"}`, Finished: true},
		},
	})
	require.NoError(t, err)

	theme := styles.DefaultStyles()
	com := &common.Common{
		App:    &app.App{Sessions: sessions, Messages: messages, History: historyService, FileTracker: fileTracker},
		Styles: &theme,
	}
	ui := &UI{
		com:     com,
		chat:    NewChat(com),
		session: &parent,
	}

	_ = ui.setSessionMessages([]message.Message{assistantMsg})

	taskNodeID := chat.TaskNodeItemID("call-general", "task-a")
	taskNode, ok := ui.chat.MessageItem(taskNodeID).(*chat.TaskNodeItem)
	require.True(t, ok)
	require.Len(t, taskNode.NestedTools(), 2)

	nestedToolIDs := []string{
		taskNode.NestedTools()[0].ToolCall().ID,
		taskNode.NestedTools()[1].ToolCall().ID,
	}
	require.ElementsMatch(t, []string{"base-view", "retry-view"}, nestedToolIDs)

	_ = ui.setSessionMessages([]message.Message{assistantMsg})
	taskNode, ok = ui.chat.MessageItem(taskNodeID).(*chat.TaskNodeItem)
	require.True(t, ok)
	require.Len(t, taskNode.NestedTools(), 2)
}

func TestUpdateLatestProposedPlanRequiresPlanModeAndPlanExit(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	com := &common.Common{Styles: &theme}
	ui := &UI{
		com:     com,
		session: &session.Session{ID: "session-1", CollaborationMode: session.CollaborationModeDefault},
	}

	planMsg := message.Message{
		ID:        "assistant-1",
		SessionID: "session-1",
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: planmode.WrapProposedPlan("- Step 1")},
			message.ToolCall{ID: "tool-1", Name: agenttools.PlanExitToolName, Finished: true},
		},
	}

	ui.updateLatestProposedPlan(planMsg)
	require.Empty(t, ui.latestProposedPlan)

	ui.session.CollaborationMode = session.CollaborationModePlan
	planMsg.Parts = []message.ContentPart{message.TextContent{Text: planmode.WrapProposedPlan("- Step 1")}}
	ui.updateLatestProposedPlan(planMsg)
	require.Empty(t, ui.latestProposedPlan)

	planMsg.Parts = append(planMsg.Parts, message.ToolCall{ID: "tool-2", Name: agenttools.PlanExitToolName, Finished: true})
	ui.updateLatestProposedPlan(planMsg)
	require.Equal(t, "- Step 1", ui.latestProposedPlan)
}

func TestStatusErrorPersistsAndIgnoresStaleClear(t *testing.T) {
	t.Parallel()

	ui := newStatusTestUI()

	_, oldCmd := ui.Update(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "old", TTL: time.Nanosecond})
	staleClear, ok := firstClearStatusMsg(oldCmd)
	require.True(t, ok)

	_, errCmd := ui.Update(util.InfoMsg{Type: util.InfoTypeError, Msg: "broken"})
	require.False(t, ui.status.msg.IsEmpty())
	require.Equal(t, util.InfoTypeError, ui.status.msg.Type)
	_, hasClear := firstClearStatusMsg(errCmd)
	require.False(t, hasClear)

	_, _ = ui.Update(staleClear)
	require.False(t, ui.status.msg.IsEmpty())
	require.Equal(t, util.InfoTypeError, ui.status.msg.Type)
}

func TestSendMessageClearsPersistentErrorStatus(t *testing.T) {
	t.Parallel()

	ui := newStatusTestUI()
	ui.session = &session.Session{ID: "s1", CollaborationMode: session.CollaborationModeDefault}
	ui.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeError, Msg: "broken"})
	ui.statusMsgSeq = 9

	cmd := ui.sendMessage("next prompt")
	require.NotNil(t, cmd)
	require.True(t, ui.status.msg.IsEmpty())
	require.Equal(t, uint64(10), ui.statusMsgSeq)
}

func TestStatusNonErrorStillSchedulesAndClearsBySeq(t *testing.T) {
	t.Parallel()

	ui := newStatusTestUI()

	_, cmd := ui.Update(util.InfoMsg{Type: util.InfoTypeWarn, Msg: "warn", TTL: time.Nanosecond})
	clearMsg, ok := firstClearStatusMsg(cmd)
	require.True(t, ok)
	require.Equal(t, ui.statusMsgSeq, clearMsg.Seq)
	require.Equal(t, util.InfoTypeWarn, ui.status.msg.Type)

	_, _ = ui.Update(clearMsg)
	require.True(t, ui.status.msg.IsEmpty())
}

func newStatusTestUI() *UI {
	theme := styles.DefaultStyles()
	com := &common.Common{
		App:    &app.App{AgentCoordinator: &mockQueueCoordinator{}},
		Styles: &theme,
	}
	return &UI{
		com:    com,
		status: NewStatus(com, nil),
		attachments: attachments.New(
			attachments.NewRenderer(
				com.Styles.Attachments.Normal,
				com.Styles.Attachments.Deleting,
				com.Styles.Attachments.Image,
				com.Styles.Attachments.Text,
			),
			attachments.Keymap{},
		),
	}
}

func firstClearStatusMsg(cmd tea.Cmd) (util.ClearStatusMsg, bool) {
	if cmd == nil {
		return util.ClearStatusMsg{}, false
	}
	msg := cmd()
	switch msg := msg.(type) {
	case util.ClearStatusMsg:
		return msg, true
	case tea.BatchMsg:
		for _, sub := range msg {
			if clear, ok := firstClearStatusMsg(sub); ok {
				return clear, true
			}
		}
	}
	return util.ClearStatusMsg{}, false
}

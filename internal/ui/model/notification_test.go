package model

import (
	"context"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/notification"
	"github.com/stretchr/testify/require"
)

type testNotificationBackend struct {
	notifications []notification.Notification
}

func (b *testNotificationBackend) Send(n notification.Notification) error {
	b.notifications = append(b.notifications, n)
	return nil
}

func runCmdRecursively(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			subCmdVal := v.Index(i)
			if subCmd, ok := subCmdVal.Interface().(tea.Cmd); ok {
				runCmdRecursively(subCmd)
			}
		}
	}
}

func TestHandleAgentNotificationUsesFinishedTurnTitle(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true

	cmd := ui.handleAgentNotification(notify.Notification{
		SessionID:    "session-1",
		SessionTitle: "demo",
		Type:         notify.TypeAgentFinished,
	})
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Len(t, backend.notifications, 1)
	require.Equal(t, "Crush finished turn", backend.notifications[0].Title)
	require.Contains(t, backend.notifications[0].Message, "Agent's turn completed")
}

func TestUpdatePermissionRequestNotificationKeepsWaitingTitle(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true

	_, cmd := ui.Update(pubsub.Event[permission.PermissionRequest]{
		Type: pubsub.CreatedEvent,
		Payload: permission.PermissionRequest{
			ToolName: "read",
		},
	})
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Len(t, backend.notifications, 1)
	require.Equal(t, "Crush is waiting...", backend.notifications[0].Title)
	require.Contains(t, backend.notifications[0].Message, "Permission required")
}

func TestHandleAgentNotificationSubagentAutoWakeup(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true
	ui.state = uiChat
	ui.focus = uiFocusMain

	sess, err := ui.com.App.Sessions.Create(context.Background(), "subagent-session")
	require.NoError(t, err)
	ui.session = &sess

	var capturedPrompt string
	mockCoord := &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		runFunc: func(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
			capturedPrompt = prompt
			return nil, nil
		},
	}
	ui.com.App.AgentCoordinator = mockCoord

	cmd := ui.handleAgentNotification(notify.Notification{
		SessionID:    sess.ID,
		SessionTitle: "demo",
		Type:         notify.TypeSubagentFinished,
		SubagentID:   "sub-1",
		Summary:      "<subagent_report>All completed successfully</subagent_report>",
	})

	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Len(t, backend.notifications, 1)
	require.Contains(t, backend.notifications[0].Message, "Subagent task sub-1 completed")
	require.Equal(t, "<subagent_report>All completed successfully</subagent_report>", capturedPrompt)
}

func TestHandleAgentNotificationSubagentQueued(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true
	ui.state = uiChat
	ui.focus = uiFocusEditor
	ui.textarea.SetValue("User draft")

	sess, err := ui.com.App.Sessions.Create(context.Background(), "subagent-session")
	require.NoError(t, err)
	ui.session = &sess

	cmd := ui.handleAgentNotification(notify.Notification{
		SessionID:    sess.ID,
		SessionTitle: "demo",
		Type:         notify.TypeSubagentFinished,
		SubagentID:   "sub-1",
		Summary:      "<subagent_report>Queued result</subagent_report>",
	})

	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Len(t, backend.notifications, 1)
	require.Contains(t, backend.notifications[0].Message, "Notification has been queued")
	require.Len(t, ui.pendingSubagentNotifications[sess.ID], 1)
	require.Equal(t, "<subagent_report>Queued result</subagent_report>", ui.pendingSubagentNotifications[sess.ID][0])
}

type mockRunCoordinator struct {
	agent.Coordinator
	busy    bool
	runFunc func(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

func (m *mockRunCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if m.runFunc != nil {
		return m.runFunc(ctx, sessionID, prompt, attachments...)
	}
	return nil, nil
}

func (m *mockRunCoordinator) CancelAll() {
	if m.Coordinator != nil {
		m.Coordinator.CancelAll()
	}
}

func (m *mockRunCoordinator) IsSessionBusy(sessionID string) bool {
	return m.busy
}

func (m *mockRunCoordinator) QueuedPrompts(sessionID string) int {
	return 0
}

func (m *mockRunCoordinator) IsQueuePaused(sessionID string) bool {
	return false
}

func TestHandleAgentNotificationSubagentMergeInjection(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	sess, err := ui.com.App.Sessions.Create(context.Background(), "subagent-session")
	require.NoError(t, err)
	ui.session = &sess

	ui.pendingSubagentNotifications = map[string][]string{
		sess.ID: {
			"<subagent_report>Result 1</subagent_report>",
			"<subagent_report>Result 2</subagent_report>",
		},
	}

	var capturedPrompt string
	mockCoord := &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		runFunc: func(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
			capturedPrompt = prompt
			return nil, nil
		},
	}
	ui.com.App.AgentCoordinator = mockCoord

	cmd := ui.sendMessage("User message")
	require.NotNil(t, cmd)

	runCmdRecursively(cmd)

	require.Contains(t, capturedPrompt, "<subagent_report>Result 1</subagent_report>")
	require.Contains(t, capturedPrompt, "<subagent_report>Result 2</subagent_report>")
	require.Contains(t, capturedPrompt, "User message")
	require.Empty(t, ui.pendingSubagentNotifications[sess.ID])
}

func TestHandleAgentNotificationSubagentIdleAutoWakeup(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true
	ui.state = uiChat
	ui.focus = uiFocusMain

	sess, err := ui.com.App.Sessions.Create(context.Background(), "subagent-session")
	require.NoError(t, err)
	ui.session = &sess

	ui.pendingSubagentNotifications = map[string][]string{
		sess.ID: {
			"<subagent_report>Queued result</subagent_report>",
		},
	}

	var capturedPrompt string
	mockCoord := &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		runFunc: func(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
			capturedPrompt = prompt
			return nil, nil
		},
	}
	ui.com.App.AgentCoordinator = mockCoord

	cmd := ui.handleAgentNotification(notify.Notification{
		SessionID:    sess.ID,
		SessionTitle: "demo",
		Type:         notify.TypeAgentFinished,
	})

	require.NotNil(t, cmd)
	runCmdRecursively(cmd)

	require.Len(t, backend.notifications, 2)
	require.Equal(t, "Crush background task finished", backend.notifications[1].Title)
	require.Contains(t, capturedPrompt, "<subagent_report>Queued result</subagent_report>")
	require.Empty(t, ui.pendingSubagentNotifications[sess.ID])
}

func TestHandleAgentNotificationSubagentMultiSessionIsolation(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	backend := &testNotificationBackend{}
	ui.notifyBackend = backend
	ui.notifyWindowFocused = false
	ui.caps.ReportFocusEvents = true
	ui.state = uiChat
	ui.focus = uiFocusMain // 空闲且非输入状态

	// 创建两个会话
	sessA, err := ui.com.App.Sessions.Create(context.Background(), "session-a")
	require.NoError(t, err)
	sessB, err := ui.com.App.Sessions.Create(context.Background(), "session-b")
	require.NoError(t, err)

	// 当前活跃会话是 sessA
	ui.session = &sessA

	var capturedPrompt string
	mockCoord := &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		runFunc: func(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
			capturedPrompt = prompt
			return nil, nil
		},
	}
	ui.com.App.AgentCoordinator = mockCoord

	// 1. 活跃会话 sessA 收到通知 -> 触发自动唤醒
	cmd := ui.handleAgentNotification(notify.Notification{
		SessionID:  sessA.ID,
		Type:       notify.TypeSubagentFinished,
		SubagentID: "subagent-a",
		Summary:    "<subagent_report>A report</subagent_report>",
	})
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Contains(t, capturedPrompt, "<subagent_report>A report</subagent_report>")
	require.Empty(t, ui.pendingSubagentNotifications[sessA.ID])

	capturedPrompt = ""

	// 2. 非活跃会话 sessB 收到通知 -> 静默暂存（不会自动唤醒当前会话）
	cmd = ui.handleAgentNotification(notify.Notification{
		SessionID:  sessB.ID,
		Type:       notify.TypeSubagentFinished,
		SubagentID: "subagent-b",
		Summary:    "<subagent_report>B report</subagent_report>",
	})
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Empty(t, capturedPrompt) // sessA 绝对不受影响
	require.Len(t, ui.pendingSubagentNotifications[sessB.ID], 1)
	require.Equal(t, "<subagent_report>B report</subagent_report>", ui.pendingSubagentNotifications[sessB.ID][0])

	// 3. 用户在活跃会话 sessA 发送消息 -> sessA 的消息被发送，sessB 依旧暂存，且 sessA 的消息不携带 sessB 的报告
	cmd = ui.sendMessage("Hello from A")
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Equal(t, "Hello from A", capturedPrompt)
	require.Len(t, ui.pendingSubagentNotifications[sessB.ID], 1) // B 的依然保留

	// 4. 切换到 sessB，并发送消息 -> 合并并消费 B 的暂存
	ui.session = &sessB
	cmd = ui.sendMessage("Hello from B")
	require.NotNil(t, cmd)
	runCmdRecursively(cmd)
	require.Contains(t, capturedPrompt, "<subagent_report>B report</subagent_report>")
	require.Contains(t, capturedPrompt, "Hello from B")
	require.Empty(t, ui.pendingSubagentNotifications[sessB.ID])
}

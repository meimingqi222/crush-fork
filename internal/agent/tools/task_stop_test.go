package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/stretchr/testify/require"
)

func TestTaskStopToolTargetsMailboxTask(t *testing.T) {
	t.Parallel()

	service := mailbox.NewService()
	require.NoError(t, service.Open("mb-1", []string{"task-a", "task-b"}))
	tool := NewTaskStopTool(service)

	// 利用 WithDelegationMailbox 注入测试信箱环境
	ctx := toolruntime.WithDelegationMailbox(context.Background(), "mb-1")

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  TaskStopToolName,
		Input: `{"task_id":"task-b","reason":"manual stop"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "agent task-b")

	envelopes, err := service.Consume("mb-1", "task-b")
	require.NoError(t, err)
	require.Len(t, envelopes, 1)
	require.Equal(t, mailbox.EnvelopeKindStop, envelopes[0].Kind)
	require.Equal(t, "manual stop", envelopes[0].Reason)
}

func TestTaskStopToolValidatesMailboxID(t *testing.T) {
	t.Parallel()

	tool := NewTaskStopTool(mailbox.NewService())
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  TaskStopToolName,
		Input: `{"reason":"x"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No active background sub-agent session was found")
}

func TestTaskStopToolSmartRouting(t *testing.T) {
	t.Parallel()

	service := mailbox.NewService()
	require.NoError(t, service.Open("mb-auto", []string{"linter", "tester"}))
	tool := NewTaskStopTool(service)

	// 测试 1：反向智能路由 —— 传 task_id="linter" 不传 mailbox_id 和 Context
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-2",
		Name:  TaskStopToolName,
		Input: `{"task_id":"linter","reason":"auto stop"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "in mailbox mb-auto")

	// 校验 linter 是否收到了 stop 信号
	envelopes, err := service.Consume("mb-auto", "linter")
	require.NoError(t, err)
	require.Len(t, envelopes, 1)
	require.Equal(t, mailbox.EnvelopeKindStop, envelopes[0].Kind)
	require.Equal(t, "auto stop", envelopes[0].Reason)

	// 测试 2：批量垃圾回收 —— 不传 task_id，自动停止该 Session 的所有活跃信箱
	respBatch, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-3",
		Name:  TaskStopToolName,
		Input: `{"reason":"batch clean"}`,
	})
	require.NoError(t, err)
	require.Contains(t, respBatch.Content, "Stop requested for all 1 active background sub-agent sessions")
}

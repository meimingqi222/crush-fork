package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

//go:embed task_stop.md
var taskStopDescription []byte

const TaskStopToolName = "task_stop"

type TaskStopParams struct {
	TaskID string `json:"task_id,omitempty" description:"Optional agent name to cancel; omit to cancel all agents"`
	Reason string `json:"reason,omitempty" description:"Optional cancellation reason"`
}

func NewTaskStopTool(service mailbox.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskStopToolName,
		string(taskStopDescription),
		func(ctx context.Context, params TaskStopParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			mailboxID := ""
			// 1. 优先级一：从 Context 中提取 Delegation Mailbox ID
			mailboxID = toolruntime.DelegationMailboxFromContext(ctx)

			// 2. 优先级二：若从 Context 提取不到，但指定了 TaskID，则根据 TaskID 反向寻找活跃信箱
			if mailboxID == "" && service != nil && strings.TrimSpace(params.TaskID) != "" {
				taskID := strings.TrimSpace(params.TaskID)
				active := service.ActiveMailboxes()
				for mbID, agents := range active {
					for _, agent := range agents {
						if agent == taskID {
							mailboxID = mbID
							break
						}
					}
					if mailboxID != "" {
						break
					}
				}
			}

			// 3. 优先级三：若 mailboxID 仍为空，且 taskID 也是空，则尝试批量停止所有活跃子代理
			if mailboxID == "" && service != nil && strings.TrimSpace(params.TaskID) == "" {
				active := service.ActiveMailboxes()
				if len(active) > 0 {
					stoppedCount := 0
					for mbID := range active {
						_, _ = service.Stop(mbID, "", strings.TrimSpace(params.Reason))
						stoppedCount++
					}
					return fantasy.NewTextResponse(fmt.Sprintf("Stop requested for all %d active background sub-agent sessions.", stoppedCount)), nil
				}
			}

			if mailboxID == "" {
				return fantasy.NewTextResponse("Failed: No active background sub-agent session was found. If you have finished your work, please output your final response directly in the chat text body instead of calling this tool."), nil
			}
			if service == nil {
				return fantasy.NewTextResponse("Failed: Background task coordination service is not configured. If you have finished your work, please output your final response directly in the chat text body instead of calling this tool."), nil
			}

			envelope, err := service.Stop(mailboxID, strings.TrimSpace(params.TaskID), strings.TrimSpace(params.Reason))
			if err != nil {
				return fantasy.NewTextResponse(fmt.Sprintf("Failed: %s. If you intended to stop or finish your work, please output your response directly in the chat text body instead of calling this tool.", strings.TrimSpace(err.Error()))), nil
			}
			if envelope.TargetAgentID == "" {
				return fantasy.NewTextResponse(fmt.Sprintf("Stop requested for mailbox %s.", envelope.MailboxID)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Stop requested for agent %s in mailbox %s.", envelope.TargetAgentID, envelope.MailboxID)), nil
		},
	)
}

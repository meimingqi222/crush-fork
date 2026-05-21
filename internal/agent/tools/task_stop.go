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
	MailboxID string `json:"mailbox_id" description:"Mailbox identifier to request cancellation in"`
	TaskID    string `json:"task_id,omitempty" description:"Optional agent name to cancel; omit to cancel all agents"`
	Reason    string `json:"reason,omitempty" description:"Optional cancellation reason"`
}

func NewTaskStopTool(service mailbox.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskStopToolName,
		string(taskStopDescription),
		func(ctx context.Context, params TaskStopParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			mailboxID := strings.TrimSpace(params.MailboxID)
			if mailboxID == "" {
				mailboxID = toolruntime.DelegationMailboxFromContext(ctx)
			}
			if mailboxID == "" {
				return fantasy.NewTextResponse("Failed: mailbox_id is required. If no mailbox is configured in this session, you cannot use task_stop. If you intended to stop or finish your work, please output your response directly in the chat text body instead of calling this tool."), nil
			}
			if service == nil {
				return fantasy.NewTextResponse("Failed: Mailbox service is not configured. If you intended to stop or finish your work, please output your response directly in the chat text body instead of calling this tool."), nil
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

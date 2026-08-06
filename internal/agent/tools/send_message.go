package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

//go:embed send_message.md
var sendMessageDescription []byte

const SendMessageToolName = "send_message"

type SendMessageParams struct {
	MailboxID string `json:"mailbox_id" description:"Mailbox identifier to deliver messages to"`
	AgentID   string `json:"agent_id,omitempty" description:"Background agent ID or name to continue with a follow-up prompt"`
	TaskID    string `json:"task_id,omitempty" description:"Optional agent name for targeted delivery; omit to broadcast"`
	Message   string `json:"message" description:"Message content to deliver"`
}

func NewSendMessageTool(service mailbox.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SendMessageToolName,
		string(sendMessageDescription),
		func(ctx context.Context, params SendMessageParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			message := strings.TrimSpace(params.Message)
			if message == "" {
				return fantasy.NewTextErrorResponse("message is required"), nil
			}

			agentID := strings.TrimSpace(params.AgentID)
			mailboxID := strings.TrimSpace(params.MailboxID)
			if mailboxID == "" {
				mailboxID = toolruntime.DelegationMailboxFromContext(ctx)
			}

			messenger := toolruntime.BackgroundAgentMessengerFromContext(ctx)
			if messenger == nil && mailboxID == "" {
				return fantasy.NewTextResponse("Failed: There are no active background subagents or mailboxes configured in the current session. You are running as a standalone primary agent. If you want to communicate with the user, please output your response directly in the chat text body instead of calling this tool."), nil
			}

			if agentID != "" {
				if messenger == nil {
					return fantasy.NewTextResponse("Failed: Background agent messaging is not available because no background subagents have been spawned. If you want to talk to the user, write your response directly in the text body instead of calling this tool."), nil
				}
				disposition, found, err := messenger(ctx, agentID, message)
				if err != nil {
					return fantasy.NewTextResponse(formatMessengerError(agentID, err)), nil
				}
				if !found {
					return fantasy.NewTextResponse(fmt.Sprintf("Failed: agent %q not found (unknown ID or name). Please ensure that the agent ID or name is correct and currently active.", agentID)), nil
				}
				switch disposition {
				case "queued":
					return fantasy.NewTextResponse(fmt.Sprintf("Follow-up prompt queued for background agent %s.", agentID)), nil
				case "started":
					return fantasy.NewTextResponse(fmt.Sprintf("Follow-up prompt sent to background agent %s.", agentID)), nil
				default:
					// The AgentRegistry fallback path (foreground subagents:
					// idle/parked revive, running-turn queueing) returns the
					// subagent's own response text as the disposition
					// instead of one of the two background-agent sentinels
					// above, since a synchronous revive has an actual
					// answer to show rather than just an accepted/queued
					// acknowledgement. See coordinator.backgroundAgentMessenger.
					return fantasy.NewTextResponse(disposition), nil
				}
			}

			if mailboxID == "" {
				return fantasy.NewTextResponse("Failed: mailbox_id is required. If no mailbox is configured in this session, you cannot use send_message. If you intended to talk to the user, write your response directly in the text body instead of calling this tool."), nil
			}
			if service == nil {
				return fantasy.ToolResponse{}, fmt.Errorf("mailbox service is not configured")
			}

			envelope, err := service.Send(mailboxID, strings.TrimSpace(params.TaskID), message)
			if err != nil {
				return fantasy.NewTextErrorResponse(strings.TrimSpace(err.Error())), nil
			}
			if envelope.TargetAgentID == "" {
				return fantasy.NewTextResponse(fmt.Sprintf("Message sent to mailbox %s.", envelope.MailboxID)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Message sent to agent %s in mailbox %s.", envelope.TargetAgentID, envelope.MailboxID)), nil
		},
	)
}

// formatMessengerError classifies a BackgroundAgentMessenger error into one
// of four categories (docs/refactor-subagent-continuation.md §7 C7:
// unknown/ambiguous/aborted/queue-full -- "unknown" is handled separately by
// the found=false branch, not an error) using the toolruntime sentinels the
// messenger wraps its errors with, rather than string-matching agent-package
// error text this package cannot import. Errors that don't match any
// sentinel (e.g. a provider failure surfaced during a synchronous revive)
// fall through to the raw message so nothing is silently swallowed.
func formatMessengerError(agentID string, err error) string {
	switch {
	case errors.Is(err, toolruntime.ErrAgentAmbiguous):
		return fmt.Sprintf("Failed: %s", strings.TrimSpace(err.Error()))
	case errors.Is(err, toolruntime.ErrAgentAborted):
		return fmt.Sprintf("Failed: agent %q failed or was stopped and cannot be resumed; spawn a new one.", agentID)
	case errors.Is(err, toolruntime.ErrAgentQueueFull):
		return fmt.Sprintf("Failed: agent %q's follow-up queue is full; wait for it to drain before sending more.", agentID)
	default:
		return fmt.Sprintf("Failed: %s", strings.TrimSpace(err.Error()))
	}
}

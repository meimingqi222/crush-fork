package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/google/uuid"
)

//go:embed irc.md
var ircDescription []byte

const IrcToolName = "irc"

type IrcOp string

const (
	IrcOpSend IrcOp = "send"
	IrcOpList IrcOp = "list"
)

type IrcParams struct {
	Op      IrcOp  `json:"op" description:"Operation: send a message or list visible peers"`
	To      string `json:"to,omitempty" description:"Recipient agent ID or 'all' to broadcast; omit for list"`
	Message string `json:"message,omitempty" description:"Message body to send (required for send)"`
	// AwaitReply is a pointer so "omitted" and "explicitly false" can be
	// told apart: a bare bool defaults to its zero value (false) whether
	// the caller passed nothing or passed false, which previously made
	// await_reply=false unreachable for DMs (see executeIrcSend). nil means
	// "use the default"; a non-nil value is always honored as-is.
	AwaitReply *bool `json:"await_reply,omitempty" description:"Wait for a reply from the recipient. Omit to use the default (true for a DM, false for a broadcast); pass true or false to force that behavior explicitly."`
}

type IrcReply struct {
	From string `json:"from"`
	Text string `json:"text"`
}

type IrcPeerInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	ParentID    string `json:"parent_id,omitempty"`
}

// IrcResponder generates a reply on behalf of the recipient (to). from is
// the sending agent's ID and must be threaded through to whatever generates
// the reply so the recipient's prompt correctly attributes the message (see
// docs/refactor-irc.md §2.1(a) -- a prior version of this callback only took
// one agent ID and the caller wired it up backwards, so recipients were told
// they'd received a message from themselves).
type IrcResponder func(ctx context.Context, from, to, message string) (string, error)

type IrcRegistry interface {
	Get(id string) (IrcPeerInfo, bool)
	ListVisibleTo(id string) []IrcPeerInfo
}

func NewIrcTool(registry IrcRegistry) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		IrcToolName,
		string(ircDescription),
		func(ctx context.Context, params IrcParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			selfID := toolruntime.IrcAgentIDFromContext(ctx)
			if selfID == "" {
				return fantasy.NewTextResponse("Failed: IRC is not available in the current context because you are running as a standalone primary agent. If you want to communicate with the user, please output your response directly in the chat text body instead of calling this tool."), nil
			}
			switch params.Op {
			case IrcOpSend:
				return executeIrcSend(ctx, registry, selfID, params)
			case IrcOpList:
				return executeIrcList(registry, selfID)
			default:
				return fantasy.NewTextResponse(fmt.Sprintf("Failed: unknown op %q; use send or list", params.Op)), nil
			}
		},
	)
}

func executeIrcSend(ctx context.Context, registry IrcRegistry, selfID string, params IrcParams) (fantasy.ToolResponse, error) {
	message := strings.TrimSpace(params.Message)
	if message == "" {
		return fantasy.NewTextResponse("Failed: message is required for send"), nil
	}

	to := strings.TrimSpace(params.To)
	if to == "" {
		return fantasy.NewTextResponse("Failed: to is required for send; use an agent ID or 'all'"), nil
	}

	// Default: wait for a DM's reply, but not a broadcast's. An explicit
	// await_reply (true or false) always overrides the default -- this is
	// the whole point of AwaitReply being *bool rather than bool: a bare
	// bool cannot distinguish "not passed" from "passed as false", which
	// used to make await_reply=false unreachable for DMs.
	awaitReply := to != "all"
	if params.AwaitReply != nil {
		awaitReply = *params.AwaitReply
	}

	var targets []IrcPeerInfo
	if to == "all" {
		targets = registry.ListVisibleTo(selfID)
	} else {
		peer, ok := registry.Get(to)
		if !ok {
			return fantasy.NewTextResponse(fmt.Sprintf("Failed: agent %q not found", to)), nil
		}
		if peer.Status != "running" && peer.Status != "idle" {
			return fantasy.NewTextResponse(fmt.Sprintf("Failed: agent %q is %s, not reachable", to, peer.Status)), nil
		}
		if peer.ID == selfID {
			return fantasy.NewTextResponse("Failed: cannot send a message to yourself"), nil
		}
		targets = []IrcPeerInfo{peer}
	}

	if len(targets) == 0 {
		return fantasy.NewTextResponse("No reachable peers found."), nil
	}

	var delivered []string
	var replies []IrcReply
	var failed []string

	responder := getIrcResponder()

	for _, target := range targets {
		// messageID exists purely for log correlation right now -- there is
		// no persisted IRCMessage envelope yet (that lands with the message
		// bus in docs/refactor-irc.md's phase 1). It still lets "sent" and
		// "delivered/failed" log lines for the same send be joined.
		messageID := uuid.New().String()[:8]
		deliveredMsg := fmt.Sprintf("[IRC `%s` → you]\n\n%s", selfID, message)
		slog.Debug("IRC message send started",
			"message_id", messageID, "from", selfID, "to", target.ID, "await_reply", awaitReply)

		if awaitReply && responder != nil {
			start := time.Now()
			replyText, err := responder(ctx, selfID, target.ID, deliveredMsg)
			elapsed := time.Since(start)
			if err != nil {
				slog.Warn("IRC message delivery failed",
					"message_id", messageID, "from", selfID, "to", target.ID, "elapsed", elapsed, "error", err)
				failed = append(failed, fmt.Sprintf("%s: %s", target.ID, err.Error()))
				continue
			}
			slog.Debug("IRC message delivered with reply",
				"message_id", messageID, "from", selfID, "to", target.ID, "elapsed", elapsed, "reply_chars", len([]rune(replyText)))
			delivered = append(delivered, target.ID)
			replies = append(replies, IrcReply{
				From: target.ID,
				Text: replyText,
			})
		} else {
			slog.Debug("IRC message delivered without waiting for a reply",
				"message_id", messageID, "from", selfID, "to", target.ID)
			delivered = append(delivered, target.ID)
		}
	}

	var result strings.Builder
	if len(delivered) > 0 {
		result.WriteString(fmt.Sprintf("Message delivered to %d peer(s): %s", len(delivered), strings.Join(delivered, ", ")))
	}
	if len(replies) > 0 {
		result.WriteString("\n\nReplies:")
		for _, reply := range replies {
			result.WriteString(fmt.Sprintf("\n- %s: %s", reply.From, truncateReply(reply.Text, 500)))
		}
	}
	if len(failed) > 0 {
		result.WriteString(fmt.Sprintf("\n\nFailed: %s", strings.Join(failed, ", ")))
	}

	return fantasy.NewTextResponse(result.String()), nil
}

func executeIrcList(registry IrcRegistry, selfID string) (fantasy.ToolResponse, error) {
	peers := registry.ListVisibleTo(selfID)

	var result strings.Builder
	if len(peers) == 0 {
		result.WriteString("No other live agents found.")
	} else {
		result.WriteString("Visible peers:\n")
		for _, peer := range peers {
			parentInfo := ""
			if peer.ParentID != "" {
				parentInfo = fmt.Sprintf(", parent=%s", peer.ParentID)
			}
			result.WriteString(fmt.Sprintf("- `%s` — %s (%s, %s%s)\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status, parentInfo))
		}
	}

	return fantasy.NewTextResponse(result.String()), nil
}

func truncateReply(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

var (
	globalResponder IrcResponder
	responderMu     sync.RWMutex
)

func SetIrcResponder(responder IrcResponder) {
	responderMu.Lock()
	defer responderMu.Unlock()
	globalResponder = responder
}

func getIrcResponder() IrcResponder {
	responderMu.RLock()
	defer responderMu.RUnlock()
	return globalResponder
}

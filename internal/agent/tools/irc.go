package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

//go:embed irc.md
var ircDescription []byte

const IrcToolName = "irc"

type IrcOp string

const (
	IrcOpSend IrcOp = "send"
	IrcOpWait IrcOp = "wait"
	IrcOpList IrcOp = "list"
)

type IrcParams struct {
	Op      IrcOp  `json:"op" description:"Operation: send a message, wait for a reply, or list visible peers"`
	To      string `json:"to,omitempty" description:"Recipient agent ID or 'all' to broadcast; omit for list"`
	Message string `json:"message,omitempty" description:"Message body to send (required for send)"`
	// AwaitReply is a pointer so "omitted" and "explicitly false" can be
	// told apart: a bare bool defaults to its zero value (false) whether
	// the caller passed nothing or passed false, which previously made
	// await_reply=false unreachable for DMs (see executeIrcSend). nil means
	// "use the default"; a non-nil value is always honored as-is.
	AwaitReply *bool `json:"await_reply,omitempty" description:"Wait for a reply from the recipient. Omit to use the default (true for a DM, false for a broadcast); pass true or false to force that behavior explicitly."`
	// ReplyTo correlates this send with an earlier inbound message's ID,
	// telling the recipient (and any sender blocked on op=wait) which
	// message this is answering. Optional; only meaningful when replying
	// to a peer message you received.
	ReplyTo string `json:"reply_to,omitempty" description:"Message ID this send replies to, if any (from a message you previously received)"`
	// MessageID is used with op=wait to wait for a reply to a specific
	// message you sent. Optional with op=wait: if omitted, the wait is for
	// any inbound message from the peer specified in `to`.
	MessageID string `json:"message_id,omitempty" description:"With op=wait: the message ID whose reply you are waiting for. Omit to wait for any message from the peer."`
	// Timeout is used with op=wait to set the maximum wait duration in
	// seconds. Defaults to 60 if omitted.
	Timeout int `json:"timeout,omitempty" description:"With op=wait: maximum seconds to wait. Default 60."`
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
	// Note is an optional peer-specific hint for the model, e.g. "message
	// revives" for a parked peer (see docs/refactor-subagent-
	// continuation.md §4 phase 2 item 1): parked peers are addressable but
	// sending to one triggers a cold revive instead of an instant delivery,
	// and the model should know that before it waits on a reply.
	Note string `json:"note,omitempty"`
}

type IrcRegistry interface {
	Get(id string) (IrcPeerInfo, bool)
	ListVisibleTo(id string) []IrcPeerInfo
}

// IrcSendRequest is a single-target delivery request handed to an IrcSender.
// It mirrors the coordinator-owned message envelope (docs/refactor-irc.md
// §3.2's IRCMessage) but stays in the tools package so this file has no
// dependency on the agent package's concrete bus type.
type IrcSendRequest struct {
	From        string
	To          string
	Body        string
	ReplyTo     string
	ExpectReply bool
}

// IrcSendResult is the tool-facing projection of a delivery receipt
// (docs/refactor-irc.md §3.2's IRCDeliveryReceipt). Outcome is one of the
// agent package's IRCDeliveryOutcome string values ("injected", "woken",
// "revived", "queued", "failed") but is carried as a plain string here for
// the same reason IrcSendRequest exists: this package must not import the
// agent package that defines the enum.
type IrcSendResult struct {
	MessageID string
	To        string
	Outcome   string
	// Reply is the recipient's actual reply text, when one was produced
	// synchronously (an idle/parked peer's real revived turn) or via the
	// phase-2 waiter (a busy peer's reply from its own turn, blocked on
	// by the sender when await_reply was true). Empty when no reply is
	// available yet (e.g. timeout, or await_reply was false).
	Reply string
	Error string
}

// IrcSender delivers IRC messages via the coordinator-owned bus
// (docs/refactor-irc.md §3). It replaces the old package-level IrcResponder
// singleton: that global crossed coordinators, tests, and concurrent
// sessions, whereas a sender is constructed per-coordinator and injected
// into NewIrcTool like the registry already is.
//
// Send/Broadcast never select a provider or construct an agent turn
// themselves -- delivery to a peer that needs a real turn (an idle or
// parked subagent) routes through the coordinator's existing
// resumeSubagent, which already owns that responsibility.
type IrcSender interface {
	Send(ctx context.Context, req IrcSendRequest) IrcSendResult
	// Broadcast delivers body to every peer visible to from except parked
	// ones (docs/refactor-irc.md §4: broadcast never revives parked peers,
	// to avoid a thundering-herd of cold revives). Returns one result per
	// attempted target; parked/self peers are simply absent, not reported
	// as failures.
	Broadcast(ctx context.Context, from, body string, expectReply bool, replyTo string) []IrcSendResult
	// Wait blocks on a reply for a specific message ID, or for any inbound
	// message from a specific peer, up to the given timeout
	// (docs/refactor-irc.md §5, phase 2). The caller's steering signal is
	// consulted so a wait can be interrupted by an inbound message (§5.3
	// deadlock prevention).
	Wait(ctx context.Context, req IrcWaitRequest) IrcWaitResult
}

// IrcWaitRequest is the input for op=wait. Either MessageID or From (or
// both) may be set: MessageID waits for a specific reply correlated by
// reply_to; From waits for any inbound message from that peer.
type IrcWaitRequest struct {
	MessageID string
	From      string
	Timeout   time.Duration
}

// IrcWaitResult is the output of op=wait. Exactly one of Reply, TimedOut,
// or Interrupted is meaningful per call.
type IrcWaitResult struct {
	MessageID   string
	From        string
	Reply       string
	TimedOut    bool
	Interrupted bool
}

func NewIrcTool(registry IrcRegistry, sender IrcSender) fantasy.AgentTool {
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
			return executeIrcSend(ctx, sender, selfID, params)
		case IrcOpWait:
			return executeIrcWait(ctx, sender, selfID, params)
		case IrcOpList:
			return executeIrcList(registry, selfID)
		default:
			return fantasy.NewTextResponse(fmt.Sprintf("Failed: unknown op %q; use send, wait, or list", params.Op)), nil
		}
		},
	)
}

func executeIrcSend(ctx context.Context, sender IrcSender, selfID string, params IrcParams) (fantasy.ToolResponse, error) {
	message := strings.TrimSpace(params.Message)
	if message == "" {
		return fantasy.NewTextResponse("Failed: message is required for send"), nil
	}

	to := strings.TrimSpace(params.To)
	if to == "" {
		return fantasy.NewTextResponse("Failed: to is required for send; use an agent ID or 'all'"), nil
	}
	if to == selfID {
		return fantasy.NewTextResponse("Failed: cannot send a message to yourself"), nil
	}

	if sender == nil {
		return fantasy.NewTextResponse("Failed: the IRC message bus is not available in the current context."), nil
	}

	// Default: wait for a DM's reply, but not a broadcast's. An explicit
	// await_reply (true or false) always overrides the default -- this is
	// the whole point of AwaitReply being *bool rather than bool: a bare
	// bool cannot distinguish "not passed" from "passed as false", which
	// used to make await_reply=false unreachable for DMs.
	expectReply := to != "all"
	if params.AwaitReply != nil {
		expectReply = *params.AwaitReply
	}
	replyTo := strings.TrimSpace(params.ReplyTo)

	if to == "all" {
		results := sender.Broadcast(ctx, selfID, message, expectReply, replyTo)
		return fantasy.NewTextResponse(formatBroadcastResults(results)), nil
	}

	result := sender.Send(ctx, IrcSendRequest{
		From:        selfID,
		To:          to,
		Body:        message,
		ReplyTo:     replyTo,
		ExpectReply: expectReply,
	})
	return fantasy.NewTextResponse(formatSendResult(result)), nil
}

// formatSendResult renders a single delivery receipt for the model. Outcome
// values come from the agent package's IRCDeliveryOutcome (see IrcSendResult's
// doc comment); unrecognized values fall back to a generic "delivered"
// phrasing rather than erroring, so a future outcome value degrades
// gracefully instead of breaking the tool.
func formatSendResult(result IrcSendResult) string {
	if result.Outcome == "failed" || (result.Outcome == "" && result.Error != "") {
		return fmt.Sprintf("Failed: %s", result.Error)
	}
	var b strings.Builder
	switch result.Outcome {
	case "injected":
		fmt.Fprintf(&b, "Message delivered to %s and queued for their attention at their next safe point.", result.To)
	case "woken":
		fmt.Fprintf(&b, "Message delivered to %s; it was idle, so it ran a full turn to answer.", result.To)
	case "revived":
		fmt.Fprintf(&b, "Message delivered to %s; it had gone dormant (parked) and was revived to answer.", result.To)
	case "queued":
		fmt.Fprintf(&b, "Message queued for %s. It is idle and will see this on its own next turn; no reply is available yet.", result.To)
	default:
		fmt.Fprintf(&b, "Message delivered to %s.", result.To)
	}
	if result.Reply != "" {
		fmt.Fprintf(&b, "\n\nReply from %s: %s", result.To, truncateReply(result.Reply, 500))
	}
	return b.String()
}

func formatBroadcastResults(results []IrcSendResult) string {
	if len(results) == 0 {
		return "No reachable peers found."
	}
	var delivered, failed []string
	var replies []IrcReply
	for _, r := range results {
		if r.Outcome == "failed" {
			failed = append(failed, fmt.Sprintf("%s: %s", r.To, r.Error))
			continue
		}
		delivered = append(delivered, r.To)
		if r.Reply != "" {
			replies = append(replies, IrcReply{From: r.To, Text: r.Reply})
		}
	}

	var b strings.Builder
	if len(delivered) > 0 {
		fmt.Fprintf(&b, "Message delivered to %d peer(s): %s", len(delivered), strings.Join(delivered, ", "))
	}
	if len(replies) > 0 {
		b.WriteString("\n\nReplies:")
		for _, reply := range replies {
			fmt.Fprintf(&b, "\n- %s: %s", reply.From, truncateReply(reply.Text, 500))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n\nFailed: %s", strings.Join(failed, ", "))
	}
	if b.Len() == 0 {
		return "No reachable peers found."
	}
	return b.String()
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
			note := ""
			if peer.Note != "" {
				note = fmt.Sprintf(" — %s", peer.Note)
			}
			result.WriteString(fmt.Sprintf("- `%s` — %s (%s, %s%s)%s\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status, parentInfo, note))
		}
	}

	return fantasy.NewTextResponse(result.String()), nil
}

// executeIrcWait implements op=wait: block on a reply to a specific message
// ID, or on any inbound message from a specific peer, up to a timeout
// (docs/refactor-irc.md §5, phase 2). The wait is interruptible by the
// caller's steering signal (§5.3: a blocked agent that receives an inbound
// message must wake up so the ring can unwind in one step, not by timeout).
func executeIrcWait(ctx context.Context, sender IrcSender, selfID string, params IrcParams) (fantasy.ToolResponse, error) {
	if sender == nil {
		return fantasy.NewTextResponse("Failed: the IRC message bus is not available in the current context."), nil
	}

	timeout := time.Duration(params.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	req := IrcWaitRequest{
		MessageID: strings.TrimSpace(params.MessageID),
		From:      strings.TrimSpace(params.To),
		Timeout:   timeout,
	}
	if req.MessageID == "" && req.From == "" {
		return fantasy.NewTextResponse("Failed: op=wait requires either message_id (to wait for a specific reply) or to (to wait for any message from that peer)."), nil
	}

	result := sender.Wait(ctx, req)
	return fantasy.NewTextResponse(formatWaitResult(result)), nil
}

func formatWaitResult(result IrcWaitResult) string {
	if result.Interrupted {
		return "Wait interrupted by an incoming message. Check your steering queue and respond if needed."
	}
	if result.TimedOut {
		if result.MessageID != "" {
			return fmt.Sprintf("Timed out waiting for a reply to message %s. The message was delivered; a late reply may still arrive -- use op=wait again to check.", result.MessageID)
		}
		return fmt.Sprintf("Timed out waiting for a message from %s.", result.From)
	}
	if result.Reply != "" {
		return fmt.Sprintf("Reply from %s: %s", result.From, truncateReply(result.Reply, 500))
	}
	return "Wait completed with no reply."
}

func truncateReply(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

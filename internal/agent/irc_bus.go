package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	agentNotify "github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// This file implements docs/refactor-irc.md's phase 1: a coordinator-owned
// message bus that replaces the tools.SetIrcResponder global singleton. It
// resolves addresses against the AgentRegistry, decides how to reach a peer
// given its current status, and reports a delivery outcome -- it
// deliberately does NOT select a provider or construct an agent turn itself.
// Turns are always started through the coordinator's existing entry points
// (resumeSubagent for idle/parked subagents; SessionAgent.EnqueueIRC, which
// rides the steering queue, for running peers and the idle primary agent).
//
// Phase 2 (not implemented here) adds a real waiter so a sender can block on
// a specific reply by message ID -- see IRCMessage.ReplyTo and the
// await_reply legacy fallback below for what phase 1 does in its place.

// IRCMessage is the message envelope handed between the bus and its
// delivery paths (docs/refactor-irc.md §3.2).
type IRCMessage struct {
	ID          string
	From        string
	To          string
	Body        string
	ReplyTo     string
	ExpectReply bool
	CreatedAt   time.Time
}

// IRCDeliveryOutcome describes how a message reached (or failed to reach)
// its target. It never implies an automatically generated reply exists --
// see IRCDeliveryReceipt.Reply for that.
type IRCDeliveryOutcome string

const (
	// IRCInjected means the message was placed on a running peer's steering
	// queue for delivery at its next safe drain point (PrepareStep).
	IRCInjected IRCDeliveryOutcome = "injected"
	// IRCWoken means an idle subagent was revived (warm, if still within
	// its keep-alive window) and completed a real turn to answer.
	IRCWoken IRCDeliveryOutcome = "woken"
	// IRCRevived means a parked subagent was cold-revived (its SessionAgent
	// rebuilt from the saved profile) and completed a real turn to answer.
	IRCRevived IRCDeliveryOutcome = "revived"
	// IRCQueued means the message was queued but no turn was started --
	// currently only the idle primary agent (docs/refactor-irc.md §4.1(b)):
	// waking it would spend the user's attention/tokens on a turn nobody
	// asked for, so it waits for the user's own next prompt instead.
	IRCQueued IRCDeliveryOutcome = "queued"
	// IRCFailed means delivery did not happen at all (unknown peer, aborted
	// peer, oversized message, etc).
	IRCFailed IRCDeliveryOutcome = "failed"
)

// IRCDeliveryReceipt is the bus's outcome for a single target. Reply is
// populated only when a real answer was available synchronously (a
// woken/revived peer's actual turn output, or the phase-1 legacy fallback
// documented on ircBus.legacyReply) -- it is never a fabricated stand-in for
// "no reply yet".
type IRCDeliveryReceipt struct {
	MessageID string
	To        string
	Outcome   IRCDeliveryOutcome
	Reply     string
	Error     string
}

const (
	// ircMaxConcurrentWakes bounds how many idle/parked revives (real,
	// costly agent turns) the bus will run at once, across all sends and
	// broadcasts combined. Steering-queue injections (running peers, idle
	// primary) don't consume this -- they're just an append to a slice.
	ircMaxConcurrentWakes = 4
	// ircMaxBroadcastFanout caps how many peers a single broadcast will
	// attempt, so one `irc send to=all` cannot silently fan out to an
	// unbounded and possibly still-growing peer set.
	ircMaxBroadcastFanout = 25
	// ircMaxMessageBytes caps a single message body.
	ircMaxMessageBytes = 8192
)

// ircBus is the coordinator-owned IRC message bus (docs/refactor-irc.md §3,
// phase 1). One instance per coordinator; injected into NewIrcTool in place
// of the old package-level IrcResponder singleton.
type ircBus struct {
	c       *coordinator
	wakeSem chan struct{}
}

func newIRCBus(c *coordinator) *ircBus {
	return &ircBus{
		c:       c,
		wakeSem: make(chan struct{}, ircMaxConcurrentWakes),
	}
}

// Send implements tools.IrcSender for a single DM.
func (b *ircBus) Send(ctx context.Context, req tools.IrcSendRequest) tools.IrcSendResult {
	msg := b.newMessage(req.From, req.To, req.Body, req.ReplyTo, req.ExpectReply)
	return toIrcSendResult(b.deliverOne(ctx, msg))
}

// Broadcast implements tools.IrcSender. Parked peers are excluded from the
// target set entirely (not attempted, not reported) so a broadcast never
// triggers a wave of cold revives -- see docs/refactor-irc.md §4's
// anti-thundering-herd rule. Concurrent delivery is bounded by fan-out size
// and, for any target that turns out to need a real turn, by wakeSem inside
// deliverOne.
func (b *ircBus) Broadcast(ctx context.Context, from, body string, expectReply bool, replyTo string) []tools.IrcSendResult {
	if b.c == nil || b.c.agentRegistry == nil {
		return nil
	}
	snaps := b.c.agentRegistry.snapshotVisibleTo(from)
	targets := make([]agentSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		if snap.Status == AgentStatusParked {
			continue
		}
		targets = append(targets, snap)
	}
	if len(targets) > ircMaxBroadcastFanout {
		slog.Warn("IRC broadcast fan-out truncated", "from", from, "peer_count", len(targets), "limit", ircMaxBroadcastFanout)
		targets = targets[:ircMaxBroadcastFanout]
	}
	if len(targets) == 0 {
		return nil
	}

	results := make([]tools.IrcSendResult, len(targets))
	var wg sync.WaitGroup
	for i, snap := range targets {
		wg.Add(1)
		go func(i int, to string) {
			defer wg.Done()
			msg := b.newMessage(from, to, body, replyTo, expectReply)
			results[i] = toIrcSendResult(b.deliverOne(ctx, msg))
		}(i, snap.ID)
	}
	wg.Wait()
	return results
}

func (b *ircBus) newMessage(from, to, body, replyTo string, expectReply bool) IRCMessage {
	return IRCMessage{
		ID:          generateAgentID(),
		From:        from,
		To:          to,
		Body:        body,
		ReplyTo:     replyTo,
		ExpectReply: expectReply,
		CreatedAt:   time.Now(),
	}
}

// deliverOne is the single-target dispatcher every send and broadcast target
// goes through. It looks the target up in the AgentRegistry, derives its
// effective status, and routes to the delivery path that status implies.
func (b *ircBus) deliverOne(ctx context.Context, msg IRCMessage) IRCDeliveryReceipt {
	receipt := IRCDeliveryReceipt{MessageID: msg.ID, To: msg.To}

	if len(msg.Body) > ircMaxMessageBytes {
		receipt.Outcome = IRCFailed
		receipt.Error = fmt.Sprintf("message exceeds %d byte limit", ircMaxMessageBytes)
		return receipt
	}
	if b.c == nil || b.c.agentRegistry == nil {
		receipt.Outcome = IRCFailed
		receipt.Error = "IRC is not available in the current context"
		return receipt
	}
	reg := b.c.agentRegistry

	targetRef, ok := reg.FullSnapshot(msg.To)
	if !ok {
		receipt.Outcome = IRCFailed
		receipt.Error = fmt.Sprintf("agent %q not found", msg.To)
		return receipt
	}
	status, ok := reg.EffectiveStatus(msg.To)
	if !ok {
		receipt.Outcome = IRCFailed
		receipt.Error = fmt.Sprintf("agent %q not found", msg.To)
		return receipt
	}

	slog.Debug("IRC message delivery started",
		"message_id", msg.ID, "from", msg.From, "to", msg.To, "status", status, "expect_reply", msg.ExpectReply)

	switch status {
	case AgentStatusAborted:
		receipt.Outcome = IRCFailed
		receipt.Error = fmt.Sprintf("agent %q failed or was canceled and cannot be resumed", msg.To)
		return receipt

	case AgentStatusRunning:
		agent, sessionID, err := b.resolveLiveAgent(targetRef, msg)
		if err != nil {
			receipt.Outcome = IRCFailed
			receipt.Error = err.Error()
			return receipt
		}
		receipt.Outcome = b.c.deliverIRCViaSteering(agent, sessionID, msg)
		if msg.ExpectReply && receipt.Outcome == IRCInjected {
			receipt.Reply = b.legacyReply(ctx, msg)
		}
		return receipt

	case AgentStatusIdle:
		if targetRef.Kind == AgentKindMain {
			// §4.1(b): the idle primary agent's attention belongs to the
			// user. Queue only; never start a turn nobody asked for.
			agent, sessionID, err := b.resolveLiveAgent(targetRef, msg)
			if err != nil {
				receipt.Outcome = IRCFailed
				receipt.Error = err.Error()
				return receipt
			}
			b.c.deliverIRCViaSteering(agent, sessionID, msg)
			receipt.Outcome = IRCQueued
			b.c.notifyPendingIRC(sessionID, msg)
			return receipt
		}
		return b.wake(ctx, targetRef, msg, IRCWoken)

	case AgentStatusParked:
		// Broadcast already filters Parked out of its target list; a DM
		// reaching here is the intended cold-revive path.
		return b.wake(ctx, targetRef, msg, IRCRevived)

	default:
		receipt.Outcome = IRCFailed
		receipt.Error = fmt.Sprintf("agent %q has unrecognized status %q", msg.To, status)
		return receipt
	}
}

// wake performs a coordinator-controlled revive (idle subagent or parked
// subagent/warm-or-cold) via resumeSubagent, which runs the peer's actual
// agent turn -- its own inference model, history, and tools -- and returns
// the real reply synchronously. Bounded by wakeSem so a single send or
// broadcast cannot start unbounded concurrent revives.
func (b *ircBus) wake(ctx context.Context, targetRef AgentRef, msg IRCMessage, outcome IRCDeliveryOutcome) IRCDeliveryReceipt {
	receipt := IRCDeliveryReceipt{MessageID: msg.ID, To: msg.To}
	select {
	case b.wakeSem <- struct{}{}:
		defer func() { <-b.wakeSem }()
	case <-ctx.Done():
		receipt.Outcome = IRCFailed
		receipt.Error = ctx.Err().Error()
		return receipt
	}

	resp, err := b.c.resumeSubagent(ctx, targetRef, formatPeerMessageForRevive(msg))
	if err != nil {
		receipt.Outcome = IRCFailed
		receipt.Error = err.Error()
		return receipt
	}
	receipt.Outcome = outcome
	receipt.Reply = resp.Content
	return receipt
}

// resolveLiveAgent returns the live SessionAgent instance and session ID to
// inject msg into for a running or idle-main target.
func (b *ircBus) resolveLiveAgent(targetRef AgentRef, msg IRCMessage) (SessionAgent, string, error) {
	if targetRef.Kind == AgentKindMain {
		sessionID, err := b.resolveMainSessionID(msg)
		if err != nil {
			return nil, "", err
		}
		if b.c.currentAgent == nil {
			return nil, "", fmt.Errorf("primary agent is not available")
		}
		return b.c.currentAgent, sessionID, nil
	}
	if strings.TrimSpace(targetRef.SessionID) == "" {
		return nil, "", fmt.Errorf("agent %q has no active session", targetRef.ID)
	}
	live, ok := b.c.childSessionAgents.Load(targetRef.SessionID)
	if !ok {
		return nil, "", fmt.Errorf("agent %q is not currently active", targetRef.ID)
	}
	agent, _ := live.(SessionAgent)
	if agent == nil {
		return nil, "", fmt.Errorf("agent %q is not currently active", targetRef.ID)
	}
	return agent, targetRef.SessionID, nil
}

// resolveMainSessionID finds which chat session to deliver a to-primary
// message into. The primary agent's own AgentRegistry entry ("0-Main") has
// no SessionID -- one sessionAgent instance serves however many chat
// sessions exist -- so the target session is derived from the sender's own
// ParentSessionID instead: a subagent's ParentSessionID is exactly the chat
// session that spawned it, which is the session whose steering queue a
// message "to the orchestrator" should land on.
func (b *ircBus) resolveMainSessionID(msg IRCMessage) (string, error) {
	senderRef, ok := b.c.agentRegistry.FullSnapshot(msg.From)
	if ok && strings.TrimSpace(senderRef.ParentSessionID) != "" {
		return senderRef.ParentSessionID, nil
	}
	return "", fmt.Errorf("cannot resolve a session to deliver to %q", msg.To)
}

// legacyReply is the phase-1 fallback for await_reply=true against a busy
// (running) peer. docs/refactor-irc.md's phase 2 adds a real waiter that
// blocks on the injected message's actual reply (a real `irc send
// reply_to=...` from the peer's own turn); until that lands, a running
// peer's steering-queue injection has no synchronous answer to return; so
// this falls back to the old RespondAsBackground one-shot, no-tools,
// no-session-history background generation -- clearly worse than a real
// answer, but strictly better than an unconditional "reply pending" for
// callers that depend on the old behavior of the responder always
// producing something.
//
// This does NOT double-inject: it never touches the target's real session
// or agent context (RespondAsBackground builds its own throwaway prompt and
// discards the fantasy.Agent it constructs), so it is a purely additional,
// non-authoritative side channel alongside the real steering-queue
// injection above, not a substitute for it.
//
// TODO(docs/refactor-irc.md phase 2): once the waiter lands, delete this
// method and the call site that invokes it, and route awaited replies
// through the waiter instead.
func (b *ircBus) legacyReply(ctx context.Context, msg IRCMessage) string {
	ref, ok := b.c.agentRegistry.FullSnapshot(msg.To)
	if !ok || ref.Agent == nil {
		return ""
	}
	reply, err := ref.Agent.RespondAsBackground(ctx, msg.From, msg.Body)
	if err != nil {
		slog.Warn("IRC legacy background reply failed", "message_id", msg.ID, "to", msg.To, "error", err)
		return ""
	}
	return reply
}

// notifyPendingIRC is the "UI relay" half of docs/refactor-irc.md §4.1(b):
// an idle primary agent doesn't auto-start a turn for a peer message, but
// the user should still learn one is waiting rather than discover it
// silently on their next prompt. Reuses the existing agent-notification
// pubsub (notify.TypeWarning is already wired to a status-line warning in
// the UI, see internal/ui/model/ui.go's handleAgentNotification) instead of
// adding a new UI-specific notification type and switch case for a single
// advisory message.
func (c *coordinator) notifyPendingIRC(sessionID string, msg IRCMessage) {
	if c == nil || c.notify == nil || sessionID == "" {
		return
	}
	c.notify.Publish(pubsub.CreatedEvent, agentNotify.Notification{
		SessionID: sessionID,
		Type:      agentNotify.TypeWarning,
		Summary:   fmt.Sprintf("Message from %s queued; you'll see it on your next turn.", msg.From),
	})
}

// deliverIRCViaSteering enqueues msg onto target's steering queue via
// SessionAgent.EnqueueIRC (never EnqueueSteer or coordinator.Steer -- see
// docs/refactor-irc.md §4.1(a): Steer hardcodes InitiatorType: InitiatorUser,
// which would misattribute an agent-initiated request for Copilot billing
// purposes -- internal/oauth/copilot/billing.go bills InitiatorUser and
// treats InitiatorAgent as free. IRC delivery carries InitiatorAgent
// instead, the same way internal/agent/vision.go does for its own
// internally-triggered requests). Returns IRCInjected when the target was
// busy (its next PrepareStep will drain the message into the active turn)
// or IRCQueued when it was idle (the message rides along with whatever
// starts the target's next turn -- see EnqueueIRC's doc comment).
func (c *coordinator) deliverIRCViaSteering(agent SessionAgent, sessionID string, msg IRCMessage) IRCDeliveryOutcome {
	call := SessionAgentCall{
		SessionID:     sessionID,
		Prompt:        msg.Body,
		InitiatorType: copilot.InitiatorAgent,
		PeerSteering:  true,
		PeerFrom:      msg.From,
		PeerMessageID: msg.ID,
	}
	if agent.EnqueueIRC(sessionID, call) {
		return IRCInjected
	}
	return IRCQueued
}

// formatPeerMessageForRevive wraps msg for a coordinator-controlled revive
// (idle subagent wake or parked cold-revive): the message becomes the
// prompt for a fresh, real turn, not a mid-turn interruption, so it does not
// need the "does not change your priorities" framing formatPeerSteeringPrompt
// applies to steering-queue injections.
func formatPeerMessageForRevive(msg IRCMessage) string {
	return fmt.Sprintf("[IRC message from `%s`]\n\n%s", msg.From, msg.Body)
}

func toIrcSendResult(r IRCDeliveryReceipt) tools.IrcSendResult {
	return tools.IrcSendResult{
		MessageID: r.MessageID,
		To:        r.To,
		Outcome:   string(r.Outcome),
		Reply:     r.Reply,
		Error:     r.Error,
	}
}

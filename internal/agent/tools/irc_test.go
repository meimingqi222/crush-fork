package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/stretchr/testify/require"
)

type mockIrcRegistry struct {
	peers map[string]IrcPeerInfo
}

func (m *mockIrcRegistry) Get(id string) (IrcPeerInfo, bool) {
	peer, ok := m.peers[id]
	return peer, ok
}

func (m *mockIrcRegistry) ListVisibleTo(id string) []IrcPeerInfo {
	var result []IrcPeerInfo
	for _, peer := range m.peers {
		if peer.ID == id {
			continue
		}
		if peer.Status != "running" && peer.Status != "idle" && peer.Status != "parked" {
			continue
		}
		result = append(result, peer)
	}
	return result
}

// mockIrcSender is a test double for the tools.IrcSender interface. The real
// implementation (agent.ircBus) lives in the agent package, which cannot be
// imported here (agent imports tools), so send/broadcast behavior is
// exercised against this scripted double; bus dispatch logic itself is
// covered by tests in the agent package (irc_bus_test.go).
type mockIrcSender struct {
	sendFunc      func(ctx context.Context, req IrcSendRequest) IrcSendResult
	broadcastFunc func(ctx context.Context, from, body string, expectReply bool, replyTo string) []IrcSendResult
	sendCalls     []IrcSendRequest
}

func (m *mockIrcSender) Send(ctx context.Context, req IrcSendRequest) IrcSendResult {
	m.sendCalls = append(m.sendCalls, req)
	if m.sendFunc == nil {
		return IrcSendResult{To: req.To, Outcome: "injected"}
	}
	return m.sendFunc(ctx, req)
}

func (m *mockIrcSender) Broadcast(ctx context.Context, from, body string, expectReply bool, replyTo string) []IrcSendResult {
	if m.broadcastFunc == nil {
		return nil
	}
	return m.broadcastFunc(ctx, from, body, expectReply, replyTo)
}

func ircCtx(selfID string) context.Context {
	return toolruntime.WithIrcAgentID(context.Background(), selfID)
}

func TestIrcTool_ListPeers(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main":        {ID: "0-Main", DisplayName: "Main", Kind: "main", Status: "running"},
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running", ParentID: "0-Main"},
			"0-Main::task2": {ID: "0-Main::task2", DisplayName: "Fix", Kind: "sub", Status: "idle", ParentID: "0-Main"},
			"0-Main::task3": {ID: "0-Main::task3", DisplayName: "Done", Kind: "sub", Status: "aborted", ParentID: "0-Main"},
			"0-Main::task4": {ID: "0-Main::task4", DisplayName: "Sleepy", Kind: "sub", Status: "parked", ParentID: "0-Main", Note: "message revives"},
		},
	}

	tool := NewIrcTool(registry, &mockIrcSender{})

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-1",
		Name:  IrcToolName,
		Input: `{"op":"list"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "0-Main::task1")
	require.Contains(t, resp.Content, "0-Main::task2")
	require.NotContains(t, resp.Content, "0-Main::task3")
	require.NotContains(t, resp.Content, "`0-Main` —")
	// Parked peers stay visible with a revive hint (docs/refactor-subagent-
	// continuation.md §4 phase 2 item 1).
	require.Contains(t, resp.Content, "0-Main::task4")
	require.Contains(t, resp.Content, "message revives")
}

func TestIrcTool_SendDM(t *testing.T) {
	sender := &mockIrcSender{
		sendFunc: func(ctx context.Context, req IrcSendRequest) IrcSendResult {
			require.Equal(t, "0-Main", req.From)
			require.Equal(t, "0-Main::task1", req.To)
			require.Equal(t, "What files changed?", req.Body)
			require.True(t, req.ExpectReply)
			return IrcSendResult{To: req.To, Outcome: "woken", Reply: "3 files modified"}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-2",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1","message":"What files changed?"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, sender.sendCalls, 1)
	require.Contains(t, resp.Content, "3 files modified")
}

// TestIrcTool_SendDM_ExplicitAwaitReplyFalse guards against the *bool
// regression this test file used to be blind to: a plain bool AwaitReply
// could not distinguish "not passed" (default: wait for a DM) from
// "explicitly false" (never wait), so DMs always ended up waiting. With
// *bool, an explicit false must be honored.
func TestIrcTool_SendDM_ExplicitAwaitReplyFalse(t *testing.T) {
	sender := &mockIrcSender{
		sendFunc: func(ctx context.Context, req IrcSendRequest) IrcSendResult {
			require.False(t, req.ExpectReply, "await_reply=false must be honored, not defaulted to true for a DM")
			return IrcSendResult{To: req.To, Outcome: "injected"}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-2b",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1","message":"fyi only","await_reply":false}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, sender.sendCalls, 1)
	require.NotContains(t, resp.Content, "Reply from")
}

// TestIrcTool_SendBroadcast_ExplicitAwaitReplyTrue is the mirror case: a
// broadcast normally does not wait, but an explicit true must override that
// default.
func TestIrcTool_SendBroadcast_ExplicitAwaitReplyTrue(t *testing.T) {
	var gotExpectReply bool
	sender := &mockIrcSender{
		broadcastFunc: func(ctx context.Context, from, body string, expectReply bool, replyTo string) []IrcSendResult {
			gotExpectReply = expectReply
			return []IrcSendResult{
				{To: "0-Main::task1", Outcome: "injected", Reply: "ack"},
				{To: "0-Main::task2", Outcome: "injected", Reply: "ack"},
			}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-3b",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"all","message":"status?","await_reply":true}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.True(t, gotExpectReply)
	require.Contains(t, resp.Content, "Replies")
}

func TestIrcTool_SendBroadcast(t *testing.T) {
	var gotExpectReply bool
	sender := &mockIrcSender{
		broadcastFunc: func(ctx context.Context, from, body string, expectReply bool, replyTo string) []IrcSendResult {
			gotExpectReply = expectReply
			return []IrcSendResult{
				{To: "0-Main::task1", Outcome: "injected"},
				{To: "0-Main::task2", Outcome: "injected"},
			}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-3",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"all","message":"Heads up, pushing changes"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.False(t, gotExpectReply, "broadcast defaults to not awaiting a reply")
	require.Contains(t, resp.Content, "delivered to 2 peer")
	require.NotContains(t, resp.Content, "Replies")
}

func TestIrcTool_SendToSelfReturnsError(t *testing.T) {
	tool := NewIrcTool(&mockIrcRegistry{}, &mockIrcSender{})

	resp, err := tool.Run(ircCtx("0-Main::task1"), fantasy.ToolCall{
		ID:    "call-4",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1","message":"hello"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "cannot send a message to yourself")
}

func TestIrcTool_SendToUnknownAgentReturnsError(t *testing.T) {
	sender := &mockIrcSender{
		sendFunc: func(ctx context.Context, req IrcSendRequest) IrcSendResult {
			return IrcSendResult{To: req.To, Outcome: "failed", Error: `agent "nonexistent" not found`}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-5",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"nonexistent","message":"hello"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "not found")
}

func TestIrcTool_SendWithoutMessageReturnsError(t *testing.T) {
	tool := NewIrcTool(&mockIrcRegistry{}, &mockIrcSender{})

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-6",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "message is required")
}

func TestIrcTool_SendWithoutToReturnsError(t *testing.T) {
	tool := NewIrcTool(&mockIrcRegistry{}, &mockIrcSender{})

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-7",
		Name:  IrcToolName,
		Input: `{"op":"send","message":"hello"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "to is required")
}

func TestIrcTool_InvalidOpReturnsError(t *testing.T) {
	tool := NewIrcTool(&mockIrcRegistry{}, &mockIrcSender{})

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-8",
		Name:  IrcToolName,
		Input: `{"op":"invalid"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "unknown op")
}

func TestIrcTool_NoAgentIDReturnsError(t *testing.T) {
	tool := NewIrcTool(&mockIrcRegistry{}, &mockIrcSender{})

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-9",
		Name:  IrcToolName,
		Input: `{"op":"list"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "not available")
}

// TestIrcTool_ReplyToIsThreadedToSender guards the reply_to plumbing added
// for phase-2 reply correlation (docs/refactor-irc.md §3.2/§8 phase 2): even
// though phase 1 has no waiter, the field must reach the sender so a future
// waiter (or a peer's manual correlation) has it.
func TestIrcTool_ReplyToIsThreadedToSender(t *testing.T) {
	sender := &mockIrcSender{
		sendFunc: func(ctx context.Context, req IrcSendRequest) IrcSendResult {
			require.Equal(t, "msg-abc123", req.ReplyTo)
			return IrcSendResult{To: req.To, Outcome: "injected"}
		},
	}

	tool := NewIrcTool(&mockIrcRegistry{}, sender)

	resp, err := tool.Run(ircCtx("0-Main::task1"), fantasy.ToolCall{
		ID:    "call-10",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main","message":"here's my answer","reply_to":"msg-abc123"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, sender.sendCalls, 1)
}

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
		if peer.Status != "running" && peer.Status != "idle" {
			continue
		}
		result = append(result, peer)
	}
	return result
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
			"0-Main::task3": {ID: "0-Main::task3", DisplayName: "Done", Kind: "sub", Status: "completed", ParentID: "0-Main"},
		},
	}

	tool := NewIrcTool(registry)

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
}

func TestIrcTool_SendDM(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running"},
		},
	}

	responderCalled := false
	SetIrcResponder(func(ctx context.Context, from, to, message string) (string, error) {
		responderCalled = true
		require.Equal(t, "0-Main", from)
		require.Equal(t, "0-Main::task1", to)
		require.Contains(t, message, "[IRC `0-Main` → you]")
		require.Contains(t, message, "What files changed?")
		return "3 files modified", nil
	})
	defer SetIrcResponder(nil)

	tool := NewIrcTool(registry)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-2",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1","message":"What files changed?"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.True(t, responderCalled)
	require.Contains(t, resp.Content, "delivered to 1 peer")
	require.Contains(t, resp.Content, "3 files modified")
}

// TestIrcTool_SendDM_ExplicitAwaitReplyFalse guards against the *bool
// regression this test file used to be blind to: a plain bool AwaitReply
// could not distinguish "not passed" (default: wait for a DM) from
// "explicitly false" (never wait), so DMs always ended up waiting. With
// *bool, an explicit false must be honored and the responder must not run.
func TestIrcTool_SendDM_ExplicitAwaitReplyFalse(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running"},
		},
	}

	responderCalled := false
	SetIrcResponder(func(ctx context.Context, from, to, message string) (string, error) {
		responderCalled = true
		return "should not be called", nil
	})
	defer SetIrcResponder(nil)

	tool := NewIrcTool(registry)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-2b",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"0-Main::task1","message":"fyi only","await_reply":false}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.False(t, responderCalled, "responder must not run when await_reply is explicitly false")
	require.Contains(t, resp.Content, "delivered to 1 peer")
	require.NotContains(t, resp.Content, "Replies")
}

// TestIrcTool_SendBroadcast_ExplicitAwaitReplyTrue is the mirror case: a
// broadcast normally does not wait, but an explicit true must override that
// default and invoke the responder for every target.
func TestIrcTool_SendBroadcast_ExplicitAwaitReplyTrue(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running"},
			"0-Main::task2": {ID: "0-Main::task2", DisplayName: "Fix", Kind: "sub", Status: "running"},
		},
	}

	calls := 0
	SetIrcResponder(func(ctx context.Context, from, to, message string) (string, error) {
		calls++
		return "ack", nil
	})
	defer SetIrcResponder(nil)

	tool := NewIrcTool(registry)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-3b",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"all","message":"status?","await_reply":true}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, 2, calls)
	require.Contains(t, resp.Content, "Replies")
}

func TestIrcTool_SendBroadcast(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running"},
			"0-Main::task2": {ID: "0-Main::task2", DisplayName: "Fix", Kind: "sub", Status: "running"},
		},
	}

	SetIrcResponder(nil)

	tool := NewIrcTool(registry)

	resp, err := tool.Run(ircCtx("0-Main"), fantasy.ToolCall{
		ID:    "call-3",
		Name:  IrcToolName,
		Input: `{"op":"send","to":"all","message":"Heads up, pushing changes"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "delivered to 2 peer")
	require.NotContains(t, resp.Content, "Replies")
}

func TestIrcTool_SendToSelfReturnsError(t *testing.T) {
	registry := &mockIrcRegistry{
		peers: map[string]IrcPeerInfo{
			"0-Main::task1": {ID: "0-Main::task1", DisplayName: "Explore", Kind: "sub", Status: "running"},
		},
	}

	tool := NewIrcTool(registry)

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
	registry := &mockIrcRegistry{peers: map[string]IrcPeerInfo{}}

	tool := NewIrcTool(registry)

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
	registry := &mockIrcRegistry{peers: map[string]IrcPeerInfo{}}

	tool := NewIrcTool(registry)

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
	registry := &mockIrcRegistry{peers: map[string]IrcPeerInfo{}}

	tool := NewIrcTool(registry)

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
	registry := &mockIrcRegistry{peers: map[string]IrcPeerInfo{}}

	tool := NewIrcTool(registry)

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
	registry := &mockIrcRegistry{peers: map[string]IrcPeerInfo{}}

	tool := NewIrcTool(registry)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-9",
		Name:  IrcToolName,
		Input: `{"op":"list"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "not available")
}

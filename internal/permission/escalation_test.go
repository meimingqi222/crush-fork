package permission

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEscalationBridge_RequestEscalation(t *testing.T) {
	bridge := NewEscalationBridge()
	ctx := context.Background()

	req := EscalationRequest{
		WorkerID:    "worker-1",
		WorkerName:  "researcher",
		WorkerColor: "blue",
		ToolName:    "bash",
		ToolInput:   map[string]interface{}{"command": "ls -la"},
		Description: "List files in directory",
	}

	errCh := make(chan error, 1)
	go func() {
		deadline := time.After(500 * time.Millisecond)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline:
				errCh <- fmt.Errorf("timeout waiting for pending escalation")
				return
			case <-tick.C:
				pending := bridge.GetPendingEscalations()
				if len(pending) == 0 {
					continue
				}
				if pending[0].WorkerID != "worker-1" {
					errCh <- fmt.Errorf("unexpected worker id %q", pending[0].WorkerID)
					return
				}
				err := bridge.RespondToEscalation(EscalationResponse{RequestID: pending[0].RequestID, Approved: true})
				errCh <- err
				return
			}
		}
	}()

	resp, err := bridge.RequestEscalation(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Approved)
	require.NoError(t, <-errCh)
}

func TestEscalationBridge_Timeout(t *testing.T) {
	bridge := NewEscalationBridge()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := EscalationRequest{
		WorkerID:    "worker-1",
		ToolName:    "bash",
		Description: "Test timeout",
	}

	resp, err := bridge.RequestEscalation(ctx, req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "canceled")
	require.Nil(t, resp)
	require.Empty(t, bridge.GetPendingEscalations())
}

func TestEscalationBridge_RespondNotFound(t *testing.T) {
	bridge := NewEscalationBridge()

	err := bridge.RespondToEscalation(EscalationResponse{
		RequestID: "nonexistent",
		Approved:  true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestEscalationBridgeFromContext(t *testing.T) {
	bridge := NewEscalationBridge()
	ctx := WithEscalationBridge(context.Background(), bridge)

	retrieved := EscalationBridgeFromContext(ctx)
	require.NotNil(t, retrieved)
	require.Same(t, bridge, retrieved)

	// Test without bridge
	retrieved = EscalationBridgeFromContext(context.Background())
	require.Nil(t, retrieved)
}

func TestWorkerIdentity(t *testing.T) {
	identity := WorkerIdentity{
		AgentID:         "agent-123",
		AgentName:       "researcher",
		AgentType:       "general",
		Color:           "blue",
		ParentSessionID: "parent-1",
		ChildSessionID:  "child-1",
		TaskID:          "task-1",
		ProfileName:     "general",
	}

	ctx := WithWorkerIdentity(context.Background(), identity)
	retrieved := WorkerIdentityFromContext(ctx)

	require.Equal(t, identity.AgentID, retrieved.AgentID)
	require.Equal(t, identity.AgentName, retrieved.AgentName)
	require.Equal(t, identity.Color, retrieved.Color)
	require.Equal(t, identity.ParentSessionID, retrieved.ParentSessionID)
	require.Equal(t, identity.ChildSessionID, retrieved.ChildSessionID)
	require.Equal(t, identity.TaskID, retrieved.TaskID)
	require.Equal(t, identity.ProfileName, retrieved.ProfileName)

	// Test without identity
	retrieved = WorkerIdentityFromContext(context.Background())
	require.Empty(t, retrieved.AgentID)
}

func TestShouldEscalate(t *testing.T) {
	// With worker identity
	identity := WorkerIdentity{AgentID: "worker-1"}
	ctx := WithWorkerIdentity(context.Background(), identity)

	require.True(t, ShouldEscalate(ctx, "ask"))
	require.False(t, ShouldEscalate(ctx, "allow"))
	require.False(t, ShouldEscalate(ctx, "deny"))

	// Without worker identity
	require.False(t, ShouldEscalate(context.Background(), "ask"))
}

func TestFormatWorkerBadge(t *testing.T) {
	tests := []struct {
		identity WorkerIdentity
		expected string
	}{
		{
			identity: WorkerIdentity{AgentID: "id-1", AgentName: "researcher", Color: "blue"},
			expected: "[blue] researcher",
		},
		{
			identity: WorkerIdentity{AgentID: "id-1", AgentName: "researcher"},
			expected: "researcher",
		},
		{
			identity: WorkerIdentity{AgentID: "id-1"},
			expected: "id-1",
		},
	}

	for _, tt := range tests {
		result := FormatWorkerBadge(tt.identity)
		require.Equal(t, tt.expected, result)
	}
}

func TestEscalateToLeader_NoBridge(t *testing.T) {
	ctx := context.Background()
	resp, err := EscalateToLeader(ctx, "bash", nil, "test")
	require.NoError(t, err)
	require.Nil(t, resp)
}

func TestEscalateToLeader_NoWorkerIdentity(t *testing.T) {
	bridge := NewEscalationBridge()
	ctx := WithEscalationBridge(context.Background(), bridge)

	resp, err := EscalateToLeader(ctx, "bash", nil, "test")
	require.NoError(t, err)
	require.Nil(t, resp)
}

func TestEscalateToLeader_Success(t *testing.T) {
	bridge := NewEscalationBridge()
	identity := WorkerIdentity{AgentID: "worker-1", AgentName: "researcher", ParentSessionID: "parent-1", ChildSessionID: "child-1", TaskID: "task-a", ProfileName: "general"}
	ctx := WithEscalationBridge(context.Background(), bridge)
	ctx = WithWorkerIdentity(ctx, identity)

	errCh := make(chan error, 1)
	go func() {
		deadline := time.After(500 * time.Millisecond)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline:
				errCh <- fmt.Errorf("timeout waiting for pending escalation")
				return
			case <-tick.C:
				pending := bridge.GetPendingEscalations()
				if len(pending) == 0 {
					continue
				}
				err := bridge.RespondToEscalation(EscalationResponse{
					RequestID: pending[0].RequestID,
					Approved:  true,
					Reason:    "Looks safe",
				})
				errCh <- err
				return
			}
		}
	}()

	resp, err := EscalateToLeader(ctx, "bash", map[string]string{"cmd": "ls"}, "list files")
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Approved)
	require.Equal(t, "Looks safe", resp.Reason)
	require.NoError(t, <-errCh)
}

func TestEscalateToLeader_PropagatesTaskMetadata(t *testing.T) {
	bridge := NewEscalationBridge()
	identity := WorkerIdentity{
		AgentID:         "worker-1",
		AgentName:       "researcher",
		ParentSessionID: "parent-1",
		ChildSessionID:  "child-1",
		TaskID:          "task-a",
		ProfileName:     "general",
	}
	ctx := WithEscalationBridge(context.Background(), bridge)
	ctx = WithWorkerIdentity(ctx, identity)

	resultCh := make(chan EscalationRequest, 1)
	go func() {
		pending := bridge.GetPendingEscalations()
		for len(pending) == 0 {
			time.Sleep(5 * time.Millisecond)
			pending = bridge.GetPendingEscalations()
		}
		resultCh <- pending[0]
		_ = bridge.RespondToEscalation(EscalationResponse{RequestID: pending[0].RequestID, Approved: true})
	}()

	resp, err := EscalateToLeader(ctx, "bash", map[string]string{"cmd": "ls"}, "list files")
	require.NoError(t, err)
	require.NotNil(t, resp)
	pending := <-resultCh
	require.Equal(t, "parent-1", pending.ParentSessionID)
	require.Equal(t, "child-1", pending.ChildSessionID)
	require.Equal(t, "task-a", pending.TaskID)
	require.Equal(t, "general", pending.ProfileName)
}

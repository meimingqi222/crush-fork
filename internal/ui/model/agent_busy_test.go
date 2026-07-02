package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsAgentBusyDistinguishesPostTurnBackgroundWork(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	sess, err := ui.com.App.Sessions.Create(t.Context(), "busy-session")
	require.NoError(t, err)
	ui.session = &sess

	ui.agentRunInProgress = true
	ui.sessionAgentLockSeen = false
	require.True(t, ui.isAgentBusy(), "startup window before session lock should be busy")

	ui.sessionAgentLockSeen = true
	mockCoord := &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		busy:        false,
	}
	ui.com.App.AgentCoordinator = mockCoord
	require.False(t, ui.isAgentBusy(), "post-turn background work should not block UI")

	mockCoord.busy = true
	require.True(t, ui.isAgentBusy(), "active session lock should be busy")
}

func TestIsSessionAgentBusyOnlyChecksSessionLock(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	sess, err := ui.com.App.Sessions.Create(t.Context(), "busy-session")
	require.NoError(t, err)
	ui.session = &sess
	ui.agentRunInProgress = true
	ui.sessionAgentLockSeen = true

	ui.com.App.AgentCoordinator = &mockRunCoordinator{
		Coordinator: ui.com.App.AgentCoordinator,
		busy:        false,
	}
	require.False(t, ui.isSessionAgentBusy())
	require.False(t, ui.isAgentBusy())
}

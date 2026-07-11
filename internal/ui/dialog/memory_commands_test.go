package dialog

import (
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCommandsIncludeMemoryCommandsWhenBackendConfigured verifies the
// "Memory: *" command family (docs/refactor-memory.md Phase 4) appears in
// the Commands panel when a memory backend is active -- the default config
// enables the local backend.
func TestCommandsIncludeMemoryCommandsWhenBackendConfigured(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	require.NotNil(t, com.App.MemoryBackend, "expected default config to enable a local memory backend")

	d, err := NewCommands(com, "", false, false, false, false, session.CollaborationModeDefault, session.PermissionModeDefault, session.Goal{}, 0, nil, nil)
	require.NoError(t, err)

	commands := d.defaultCommands()
	ids := make([]string, 0, len(commands))
	for _, c := range commands {
		ids = append(ids, c.ID())
	}
	require.Contains(t, ids, "memory_status")
	require.Contains(t, ids, "memory_search")
	require.Contains(t, ids, "memory_consolidate")
	require.Contains(t, ids, "memory_clear")

	searchIdx := slices.IndexFunc(commands, func(item *CommandItem) bool { return item.ID() == "memory_search" })
	require.NotEqual(t, -1, searchIdx)
	action, ok := commands[searchIdx].Action().(ActionOpenDialog)
	require.True(t, ok)
	require.Equal(t, MemorySearchID, action.DialogID)

	clearIdx := slices.IndexFunc(commands, func(item *CommandItem) bool { return item.ID() == "memory_clear" })
	require.NotEqual(t, -1, clearIdx)
	clearAction, ok := commands[clearIdx].Action().(ActionOpenDialog)
	require.True(t, ok)
	require.Equal(t, MemoryClearID, clearAction.DialogID)
}

// TestCommandsOmitMemoryCommandsWhenBackendDisabled verifies that with
// memory.backend=off, no "Memory: *" commands are registered -- mirroring
// the LLM tool-gating behavior from Phase 3 on the user-facing side.
func TestCommandsOmitMemoryCommandsWhenBackendDisabled(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true,"memory":{"backend":"off"}},"tools":{}}`)
	require.Nil(t, com.App.MemoryBackend)

	d, err := NewCommands(com, "", false, false, false, false, session.CollaborationModeDefault, session.PermissionModeDefault, session.Goal{}, 0, nil, nil)
	require.NoError(t, err)

	commands := d.defaultCommands()
	for _, c := range commands {
		require.NotContains(t, []string{"memory_status", "memory_search", "memory_consolidate", "memory_clear"}, c.ID())
	}
}

// TestMemorySearchDialogSubmitsQuery verifies the search dialog collects
// text input and returns ActionMemorySearch on Enter, and ActionClose on
// Esc.
func TestMemorySearchDialogSubmitsQuery(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	d := NewMemorySearch(com)
	require.Equal(t, MemorySearchID, d.ID())

	for _, r := range "sqlite" {
		d.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	search, ok := action.(ActionMemorySearch)
	require.True(t, ok)
	require.Equal(t, "sqlite", search.Query)

	closeAction := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, ActionClose{}, closeAction)
}

// TestMemorySearchDialogIgnoresEmptyQuery verifies Enter on an empty query
// does not submit.
func TestMemorySearchDialogIgnoresEmptyQuery(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	d := NewMemorySearch(com)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
}

// TestMemoryClearDialogDefaultsToCancel verifies the confirmation dialog
// defaults to "No" so a stray Enter does not destroy data, and that
// pressing "y" always clears regardless of the currently focused button.
func TestMemoryClearDialogDefaultsToCancel(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	d := NewMemoryClear(com)
	require.Equal(t, MemoryClearID, d.ID())
	require.True(t, d.selectedNo)

	// Default Enter (No selected) closes without clearing.
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionClose{}, action)

	// "y" always confirms clearing.
	confirmAction := d.HandleMsg(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.Equal(t, ActionMemoryClearConfirmed{}, confirmAction)

	// Toggling then confirming with Enter also clears.
	d2 := NewMemoryClear(com)
	d2.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.False(t, d2.selectedNo)
	toggledAction := d2.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionMemoryClearConfirmed{}, toggledAction)
}

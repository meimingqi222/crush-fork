package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShortHelpEditorShowsOnlyPrimaryBindings(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.keyMap = DefaultKeyMap()
	u.state = uiChat
	u.focus = uiFocusEditor
	u.textarea.SetValue("")

	binds := u.ShortHelp()
	got := make([]string, 0, len(binds))
	for _, binding := range binds {
		help := binding.Help()
		got = append(got, help.Key+" "+help.Desc)
	}

	require.Contains(t, got, "tab focus chat")
	require.Contains(t, got, "/ or ctrl+/ commands")
	require.Contains(t, got, "shift+tab cycle ask/auto/yolo")
	require.Contains(t, got, "ctrl+p enhance prompt")
	require.Contains(t, got, "ctrl+c quit")
	require.Contains(t, got, "ctrl+g more shortcuts")
	require.NotContains(t, got, "ctrl+enter newline")
	require.NotContains(t, got, "ctrl+l models")
}

func TestShortHelpMainShowsOnlyPrimaryBindings(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.keyMap = DefaultKeyMap()
	u.state = uiChat
	u.focus = uiFocusMain

	binds := u.ShortHelp()
	got := make([]string, 0, len(binds))
	for _, binding := range binds {
		help := binding.Help()
		got = append(got, help.Key+" "+help.Desc)
	}

	require.Contains(t, got, "tab focus editor")
	require.Contains(t, got, "ctrl+/ commands")
	require.Contains(t, got, "↑↓ scroll")
	require.Contains(t, got, "ctrl+g more shortcuts")
	require.NotContains(t, got, "ctrl+l models")
	require.NotContains(t, got, "ctrl+d jump message")
	require.NotContains(t, got, "ctrl+n/ctrl+r/ctrl+b navigate sessions")
}

package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	uichat "github.com/charmbracelet/crush/internal/ui/chat"
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
	require.Contains(t, got, "ctrl+/ commands")
	require.Contains(t, got, "ctrl+c quit")
	require.Contains(t, got, "ctrl+g more shortcuts")
	require.NotContains(t, got, "shift+tab cycle ask/auto/yolo")
	require.NotContains(t, got, "alt+o cycle standard/plan/orchestrate")
	require.NotContains(t, got, "ctrl+p enhance prompt")
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
	require.NotContains(t, got, "]/l open subagent")
	require.NotContains(t, got, "ctrl+l models")
	require.NotContains(t, got, "ctrl+d jump message")
	require.NotContains(t, got, "ctrl+n/ctrl+r/ctrl+b navigate sessions")
}

func TestShortHelpMainShowsOpenSubagentForTaskNode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.keyMap = DefaultKeyMap()
	u.state = uiChat
	u.focus = uiFocusMain
	node := uichat.NewTaskNodeItem(u.com.Styles, "call-agent", "review", "Review", "Review", "general", "child-session")
	u.chat.SetMessages(node)
	require.True(t, u.chat.SelectMessage(node.ID()))

	binds := u.ShortHelp()
	got := make([]string, 0, len(binds))
	for _, binding := range binds {
		help := binding.Help()
		got = append(got, help.Key+" "+help.Desc)
	}

	require.Contains(t, got, "]/l open subagent")
}

func TestShortHelpSubagentShowsOnlyTrimmedBindings(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.keyMap = DefaultKeyMap()
	u.state = uiChat
	u.focus = uiFocusMain
	u.session = &session.Session{ID: "child-1", ParentSessionID: "parent-1"}
	u.siblingCount = 3
	u.siblingIndex = 2

	binds := u.ShortHelp()
	got := make([]string, 0, len(binds))
	for _, binding := range binds {
		help := binding.Help()
		got = append(got, help.Key+" "+help.Desc)
	}

	require.Contains(t, got, "↑↓ scroll")
	require.Contains(t, got, "[/h exit subagent")
	require.Contains(t, got, "ctrl+↑ prev subagent")
	require.Contains(t, got, "ctrl+↓ next subagent")
	require.Contains(t, got, "ctrl+c quit")
	require.Contains(t, got, "ctrl+g more shortcuts")

	// Bindings that don't apply (or aren't essential) inside the read-only
	// subagent view should not clutter the trimmed short help.
	require.NotContains(t, got, "tab focus editor")
	require.NotContains(t, got, "ctrl+/ commands")
	require.NotContains(t, got, "ctrl+n new session")
	require.Len(t, got, 6)
}

func TestShortHelpSubagentHidesSiblingNavWithoutSiblings(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.keyMap = DefaultKeyMap()
	u.state = uiChat
	u.focus = uiFocusMain
	u.session = &session.Session{ID: "child-1", ParentSessionID: "parent-1"}
	u.siblingCount = 1
	u.siblingIndex = 1

	binds := u.ShortHelp()
	got := make([]string, 0, len(binds))
	for _, binding := range binds {
		help := binding.Help()
		got = append(got, help.Key+" "+help.Desc)
	}

	require.Contains(t, got, "[/h exit subagent")
	require.NotContains(t, got, "ctrl+↑ prev subagent")
	require.NotContains(t, got, "ctrl+↓ next subagent")
}
